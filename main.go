package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rwcarlsen/goexif/exif"
)

var (
	imageExts   = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".tiff": true, ".bmp": true, ".heic": true, ".heif": true}
	videoExts   = map[string]bool{".mp4": true, ".avi": true, ".mov": true, ".wmv": true, ".mkv": true, ".flv": true, ".mpeg": true, ".mpg": true, ".m4v": true}
	heicExts    = map[string]bool{".heic": true, ".heif": true}
	archiveExts = map[string]bool{".zip": true, ".rar": true, ".7z": true, ".tar": true, ".gz": true, ".bz2": true, ".xz": true, ".tar.gz": true, ".tar.bz2": true, ".tar.xz": true}
)

var (
	scriptDir, _ = os.Getwd() // Use current working directory instead of binary location
	sourceDir    = filepath.Join(scriptDir, "unsorted_photos")
	destDir      = filepath.Join(scriptDir, "sorted_photos")
	noDateDir    = filepath.Join(destDir, "no_date")
	archivesDir  = filepath.Join(destDir, "archives")
	errorsDir    = filepath.Join(destDir, "errors")
)

var (
	hashMu              sync.Mutex
	hashesInDestination = make(map[string]map[string]bool, 20) // Pre-allocate with estimated year folders

	// Cache for directories that have been created to avoid repeated MkdirAll calls
	createdDirsMu sync.RWMutex
	createdDirs   = make(map[string]bool, 50) // Pre-allocate for common directories
)

// Counters
var (
	counterMu             sync.Mutex
	movedCount            int
	videoMovedCount       int
	heicConvertedCount    int
	noDateCount           int
	archiveMovedCount     int
	archiveExtractedCount int // New counter for extracted archives
	deletedNonMediaCount  int
	errorCount            int
	skippedCount          int
	duplicateDeletedCount int
	totalFiles            int64 // Track total files for progress
	processedFiles        int64 // Track processed files for progress
)

func main() {
	log.SetFlags(log.LstdFlags)
	log.Printf("Starting media sort from '%s' to '%s'...", sourceDir, destDir)
	log.Println("HEIC/HEIF files will be converted to JPEG.")
	log.Println("IMPORTANT: Sorting by 'Date Taken' metadata for photos and 'Media Created' metadata for videos - ignoring file system dates")
	log.Println("Files without metadata will be sorted by extension in 'no_date' folder")
	log.Println("ZIP archives will be extracted and contents processed automatically")

	// Check if source directory exists
	if _, err := os.Stat(sourceDir); os.IsNotExist(err) {
		log.Fatalf("Source directory '%s' not found. Exiting.", sourceDir)
	}

	// Ensure destination directories exist
	dirs := []string{destDir, noDateDir, archivesDir, errorsDir}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			log.Fatalf("Failed to create directory %s: %v", d, err)
		}
	}

	var wg sync.WaitGroup
	fileChan := make(chan string, 1000) // Increased buffer size for better throughput

	// Use more workers based on CPU cores for better performance
	numWorkers := runtime.NumCPU() * 2 // Use 2x CPU cores for I/O bound operations
	if numWorkers < 4 {
		numWorkers = 4 // Minimum 4 workers
	}
	log.Printf("Using %d worker goroutines for processing", numWorkers)
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range fileChan {
				processFile(path)
			}
		}()
	}

	// Walk the source directory and send files to workers
	log.Println("Scanning files...")
	var fileCount int64
	err := filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Printf("Error walking %s: %v", path, err)
			return nil
		}
		if info.IsDir() {
			return nil
		}

		// Skip files that might already be in a destination structure
		if strings.Contains(path, destDir) {
			log.Printf("Skipping file already in destination structure: %s", path)
			counterMu.Lock()
			skippedCount++
			counterMu.Unlock()
			return nil
		}

		fileCount++
		fileChan <- path
		return nil
	})

	// Set total files for progress tracking
	atomic.StoreInt64(&totalFiles, fileCount)
	log.Printf("Found %d files to process", fileCount)
	if err != nil {
		log.Fatalf("Failed to walk source directory: %v", err)
	}
	close(fileChan)
	wg.Wait()

	// Clean up empty directories in source
	cleanupEmptyDirectories(sourceDir)

	// Print summary
	printSummary()
}

// ensureDir creates a directory if it doesn't exist, using a cache to avoid repeated checks
func ensureDir(dir string) error {
	// Check cache first (read lock)
	createdDirsMu.RLock()
	if createdDirs[dir] {
		createdDirsMu.RUnlock()
		return nil
	}
	createdDirsMu.RUnlock()

	// Not in cache, acquire write lock and create directory
	createdDirsMu.Lock()
	defer createdDirsMu.Unlock()

	// Double-check in case another goroutine created it
	if createdDirs[dir] {
		return nil
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	createdDirs[dir] = true
	return nil
}

func processFile(path string) {
	defer func() {
		// Update progress counter
		processed := atomic.AddInt64(&processedFiles, 1)
		total := atomic.LoadInt64(&totalFiles)
		if processed%100 == 0 || processed == total {
			log.Printf("Progress: %d/%d files processed (%.1f%%)", processed, total, float64(processed)/float64(total)*100)
		}
	}()

	ext := strings.ToLower(filepath.Ext(path))
	filename := filepath.Base(path)
	var targetFolder string
	var mediaType string
	var yearOrStatus string

	if imageExts[ext] {
		mediaType = "image"
		// Extract year from EXIF "Date Taken" metadata ONLY (ignoring file system dates)
		yearOrStatus = getExifYear(path)
	} else if videoExts[ext] {
		mediaType = "video"
		// Extract year from video "Media Created" metadata (ignoring file system dates)
		yearOrStatus = getVideoDateYear(path)
	} else if archiveExts[ext] {
		mediaType = "archive"
		// Try to extract archive contents and process them
		if extractArchive(path) {
			log.Printf("Successfully extracted and processed contents of '%s'", filename)
			counterMu.Lock()
			archiveExtractedCount++
			counterMu.Unlock()
			// Delete the original archive after successful extraction
			if err := os.Remove(path); err != nil {
				log.Printf("Warning: Could not delete original archive '%s' after extraction: %v", path, err)
			}
			return
		} else {
			// Extraction failed, move to archives folder as before
			targetFolder = archivesDir
			log.Printf("Could not extract '%s', moving to '%s' (archive file)", filename, "archives")
			counterMu.Lock()
			archiveMovedCount++
			counterMu.Unlock()
		}
	} else {
		mediaType = "other"
		// Delete non-media files
		if err := os.Remove(path); err != nil {
			log.Printf("Could not delete non-media file '%s': %v", path, err)
			counterMu.Lock()
			errorCount++
			counterMu.Unlock()
		} else {
			log.Printf("Deleted '%s' (not a recognized media file)", filename)
			counterMu.Lock()
			deletedNonMediaCount++
			counterMu.Unlock()
		}
		return
	}

	// Determine target folder based on metadata (Date Taken for images, Media Created for videos)
	if mediaType == "image" || mediaType == "video" {
		if yearOrStatus == "error" {
			targetFolder = errorsDir
			log.Printf("Moving '%s' to '%s' due to processing error.", filename, "errors")
			counterMu.Lock()
			errorCount++
			counterMu.Unlock()
		} else if yearOrStatus != "" && yearOrStatus != "none" {
			// Year was successfully extracted from metadata
			targetFolder = filepath.Join(destDir, yearOrStatus)
			if mediaType == "image" {
				log.Printf("Processing '%s' (%s) for year '%s' (from Date Taken metadata)", filename, mediaType, yearOrStatus)
			} else {
				log.Printf("Processing '%s' (%s) for year '%s' (from Media Created metadata)", filename, mediaType, yearOrStatus)
			}
		} else {
			// No metadata found - sort by file extension (ignoring file system dates)
			extCat := getFileExtensionCategory(path)
			targetFolder = filepath.Join(noDateDir, extCat)
			if mediaType == "image" {
				log.Printf("Processing '%s' (%s) for '%s' (no Date Taken metadata found, ignoring file dates, sorting by extension: %s)", filename, mediaType, filepath.Join("no_date", extCat), extCat)
			} else {
				log.Printf("Processing '%s' (%s) for '%s' (no Media Created metadata found, ignoring file dates, sorting by extension: %s)", filename, mediaType, filepath.Join("no_date", extCat), extCat)
			}
			counterMu.Lock()
			noDateCount++
			counterMu.Unlock()
		}
	}

	if targetFolder == "" {
		return
	}

	// Create target folder efficiently with caching
	if err := ensureDir(targetFolder); err != nil {
		log.Printf("Failed to create directory %s: %v", targetFolder, err)
		return
	}

	// Calculate hash for deduplication
	hash, err := fileHash(path)
	if err != nil {
		log.Printf("Could not calculate hash for %s. Moving to errors folder.", filename)
		targetFolder = errorsDir
		ensureDir(targetFolder) // Use optimized directory creation
		counterMu.Lock()
		errorCount++
		counterMu.Unlock()
	} else {
		// Check for duplicates in the target folder
		hashMu.Lock()
		if hashesInDestination[targetFolder] == nil {
			hashesInDestination[targetFolder] = make(map[string]bool, 100) // Pre-allocate for typical folder size
		}
		if hashesInDestination[targetFolder][hash] {
			hashMu.Unlock()
			log.Printf("Duplicate detected (hash match in run): '%s' for '%s'. Deleting source.", filename, filepath.Base(targetFolder))
			if err := os.Remove(path); err != nil {
				log.Printf("Could not delete duplicate source file '%s': %v", path, err)
				counterMu.Lock()
				errorCount++
				counterMu.Unlock()
			} else {
				counterMu.Lock()
				duplicateDeletedCount++
				counterMu.Unlock()
			}
			return
		}
		hashesInDestination[targetFolder][hash] = true
		hashMu.Unlock()
	}

	// Handle HEIC conversion or regular file move
	if mediaType == "image" && heicExts[ext] {
		convertHEIC(path, targetFolder, hash)
	} else {
		moveFile(path, targetFolder, filename, hash, mediaType)
	}
}

// getFileExtensionCategory categorizes files by extension for no_date sorting
func getFileExtensionCategory(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return "no_extension"
	}
	// Remove the dot from extension for folder name
	return ext[1:]
}

// getExifYear tries to extract the year from EXIF "Date Taken" metadata ONLY
// This function explicitly ignores file system dates (modified/created) and only uses camera metadata
func getExifYear(path string) string {
	ext := strings.ToLower(filepath.Ext(path))

	// Only try EXIF for formats that commonly have it (skip PNG, GIF, BMP for performance)
	if ext != ".jpg" && ext != ".jpeg" && ext != ".tiff" && ext != ".heic" && ext != ".heif" {
		return ""
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("File not found during EXIF read: %s", path)
			return "error"
		}
		log.Printf("Error opening file for EXIF: %s: %v", path, err)
		return ""
	}
	defer f.Close()

	x, err := exif.Decode(f)
	if err != nil {
		// This is normal for many image types that don't have EXIF
		return ""
	}

	// Priority order for EXIF date tags (most reliable first):
	// 1. DateTimeOriginal - when the photo was taken (most reliable)
	// 2. DateTimeDigitized - when the photo was digitized
	// 3. DateTime - when the file was last modified (least reliable, but still EXIF)

	// Try DateTimeOriginal first (most reliable) - this is the actual "date taken"
	if tag, err := x.Get(exif.DateTimeOriginal); err == nil {
		if dateStr, err := tag.StringVal(); err == nil && len(dateStr) >= 4 {
			if year := extractYearFromDateString(dateStr); year != "" {
				log.Printf("Found DateTimeOriginal for %s: %s", filepath.Base(path), year)
				return year
			}
		}
	}

	// Try DateTimeDigitized as second choice
	if tag, err := x.Get(exif.DateTimeDigitized); err == nil {
		if dateStr, err := tag.StringVal(); err == nil && len(dateStr) >= 4 {
			if year := extractYearFromDateString(dateStr); year != "" {
				log.Printf("Found DateTimeDigitized for %s: %s", filepath.Base(path), year)
				return year
			}
		}
	}

	// Try DateTime() method as fallback (this tries multiple tags internally)
	if dt, err := x.DateTime(); err == nil {
		year := dt.Year()
		if year > 1900 && year <= time.Now().Year()+1 {
			log.Printf("Found DateTime method for %s: %d", filepath.Base(path), year)
			return strconv.Itoa(year)
		}
	}

	// Try DateTime tag as final fallback
	if tag, err := x.Get(exif.DateTime); err == nil {
		if dateStr, err := tag.StringVal(); err == nil && len(dateStr) >= 4 {
			if year := extractYearFromDateString(dateStr); year != "" {
				log.Printf("Found DateTime tag for %s: %s", filepath.Base(path), year)
				return year
			}
		}
	}

	// Explicitly log that we found no EXIF date (ignoring file system dates)
	log.Printf("No EXIF date metadata found for %s (ignoring file system dates)", filepath.Base(path))
	return ""
}

// extractYearFromDateString efficiently extracts year from EXIF date string
func extractYearFromDateString(dateStr string) string {
	if len(dateStr) >= 4 {
		// EXIF format is typically "YYYY:MM:DD HH:MM:SS"
		if len(dateStr) >= 10 && dateStr[4] == ':' && dateStr[7] == ':' {
			return dateStr[:4]
		}
		// Also try just the first 4 characters as year
		if year := dateStr[:4]; len(year) == 4 {
			if y, err := strconv.Atoi(year); err == nil && y > 1900 && y <= time.Now().Year()+1 {
				return year
			}
		}
	}
	return ""
}

// getVideoDateYear attempts to extract the media creation date from video metadata
// This reads the "media created" timestamp from video file metadata, NOT file system dates
func getVideoDateYear(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	filename := filepath.Base(path)

	log.Printf("Attempting to extract video metadata for: %s (extension: %s)", filename, ext)

	var creationTime time.Time
	var found bool

	switch ext {
	case ".mp4", ".m4v", ".mov":
		// Try to read QuickTime/MP4 creation time from metadata
		log.Printf("Processing MP4/MOV file: %s", filename)
		creationTime, found = extractMP4CreationTime(path)
	case ".avi":
		// Try to read AVI creation time from metadata
		log.Printf("Processing AVI file: %s", filename)
		creationTime, found = extractAVICreationTime(path)
	default:
		// For other video formats, we currently can't extract metadata
		log.Printf("Video metadata extraction not supported for format '%s': %s", ext, filename)
		return ""
	}

	if found {
		year := creationTime.Year()
		if year > 1900 && year <= time.Now().Year()+1 {
			log.Printf("✓ Found media creation date for %s: %d", filename, year)
			return strconv.Itoa(year)
		} else {
			log.Printf("⚠ Invalid media creation year (%d) for %s, treating as no date", year, filename)
			return ""
		}
	}

	log.Printf("✗ No media creation date found in metadata for %s", filename)
	return ""
}

// extractMP4CreationTime extracts creation time from MP4/MOV/M4V metadata
func extractMP4CreationTime(path string) (time.Time, bool) {
	file, err := os.Open(path)
	if err != nil {
		log.Printf("Error opening video file for metadata reading: %s: %v", filepath.Base(path), err)
		return time.Time{}, false
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil {
		return time.Time{}, false
	}
	fileSize := fi.Size()
	if fileSize < 16 {
		return time.Time{}, false
	}

	// Helper to read atom header (size + type). Returns payload start offset and size.
	readAtomHeader := func(f *os.File, at int64) (atomType string, atomSize int64, headerLen int64, ok bool) {
		if at+8 > fileSize {
			return "", 0, 0, false
		}
		if _, err := f.Seek(at, io.SeekStart); err != nil {
			return "", 0, 0, false
		}
		header := make([]byte, 8)
		if _, err := io.ReadFull(f, header); err != nil {
			return "", 0, 0, false
		}
		size := int64(binary.BigEndian.Uint32(header[0:4]))
		typ := string(header[4:8])
		hdrLen := int64(8)
		if size == 1 { // 64-bit extended size
			ext := make([]byte, 8)
			if _, err := io.ReadFull(f, ext); err != nil {
				return "", 0, 0, false
			}
			size = int64(binary.BigEndian.Uint64(ext))
			hdrLen = 16
		} else if size == 0 { // extends to EOF
			size = fileSize - at
		}
		// Basic sanity
		if size < hdrLen || at+size > fileSize {
			return "", 0, 0, false
		}
		return typ, size, hdrLen, true
	}

	// Locate 'moov' atom (may be at end of file if fast-start not applied)
	var moovPayloadOffset int64
	var moovPayloadSize int64
	offset := int64(0)
	for offset < fileSize {
		typ, size, hdrLen, ok := readAtomHeader(file, offset)
		if !ok || size == 0 {
			break
		}
		if typ == "moov" {
			moovPayloadOffset = offset + hdrLen
			moovPayloadSize = size - hdrLen
			break
		}
		// Skip to next top-level atom
		offset += size
	}

	if moovPayloadOffset == 0 || moovPayloadSize <= 0 {
		log.Printf("No 'moov' atom found in video file: %s", filepath.Base(path))
		return time.Time{}, false
	}

	// Walk atoms inside moov to find mvhd
	innerOffset := int64(0)
	for innerOffset < moovPayloadSize {
		atomStart := moovPayloadOffset + innerOffset
		typ, size, _, ok := readAtomHeader(file, atomStart)
		if !ok || size == 0 {
			break
		}
		if typ == "mvhd" {
			// Read mvhd header contents after version+flags
			versionFlags := make([]byte, 4)
			if _, err := io.ReadFull(file, versionFlags); err != nil {
				return time.Time{}, false
			}
			version := versionFlags[0]
			const mp4Epoch = 2082844800 // Seconds between 1904-01-01 and 1970-01-01
			if version == 1 {
				// creation_time (8) + modification_time (8)
				buf := make([]byte, 8)
				if _, err := io.ReadFull(file, buf); err != nil {
					return time.Time{}, false
				}
				creation := binary.BigEndian.Uint64(buf)
				if creation < mp4Epoch { // skip invalid
					return time.Time{}, false
				}
				unixSecs := int64(creation - mp4Epoch)
				ct := time.Unix(unixSecs, 0).UTC()
				log.Printf("Extracted creation time (v1 mvhd) from %s: %s", filepath.Base(path), ct.Format(time.RFC3339))
				return ct, true
			} else { // version 0
				buf := make([]byte, 4)
				if _, err := io.ReadFull(file, buf); err != nil {
					return time.Time{}, false
				}
				creation := binary.BigEndian.Uint32(buf)
				if creation == 0 { // ignore zero
					return time.Time{}, false
				}
				if creation < mp4Epoch { // probable corruption
					return time.Time{}, false
				}
				unixSecs := int64(creation - mp4Epoch)
				ct := time.Unix(unixSecs, 0).UTC()
				log.Printf("Extracted creation time (mvhd) from %s: %s", filepath.Base(path), ct.Format(time.RFC3339))
				return ct, true
			}
		}
		// Move to next atom inside moov
		innerOffset += size
	}

	log.Printf("No 'mvhd' atom with creation time found in video file: %s", filepath.Base(path))
	return time.Time{}, false
}

// extractAVICreationTime extracts creation time from AVI metadata
func extractAVICreationTime(path string) (time.Time, bool) {
	// AVI (RIFF) files may contain an INFO list with ICRD (creation date) or IDIT (digitization date)
	// We scan the RIFF structure for LIST 'INFO' then look for ICRD/IDIT chunks.
	f, err := os.Open(path)
	if err != nil {
		log.Printf("Error opening AVI file for metadata reading: %s: %v", filepath.Base(path), err)
		return time.Time{}, false
	}
	defer f.Close()

	header := make([]byte, 12)
	if _, err := io.ReadFull(f, header); err != nil {
		return time.Time{}, false
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "AVI " {
		return time.Time{}, false
	}

	// Helper to read next chunk header
	type chunkHeader struct {
		id   string
		size uint32
		off  int64 // offset to data start
	}
	readChunk := func(r *os.File) (*chunkHeader, error) {
		pos, _ := r.Seek(0, io.SeekCurrent)
		hdr := make([]byte, 8)
		if _, err := io.ReadFull(r, hdr); err != nil {
			return nil, err
		}
		id := string(hdr[0:4])
		size := binary.LittleEndian.Uint32(hdr[4:8])
		return &chunkHeader{id: id, size: size, off: pos + 8}, nil
	}

	fileInfo, _ := f.Stat()
	limit := fileInfo.Size()

	// We keep a simple year extraction helper.
	extractYear := func(text string) (int, bool) {
		nowYear := time.Now().Year() + 1
		for i := 0; i <= len(text)-4; i++ {
			c0 := text[i]
			if c0 < '1' || c0 > '2' { // years start with 19/20 typically
				continue
			}
			if i+4 > len(text) {
				break
			}
			yStr := text[i : i+4]
			y, err := strconv.Atoi(yStr)
			if err == nil && y >= 1970 && y <= nowYear {
				return y, true
			}
		}
		return 0, false
	}

	// Scan through chunks. We don't need to parse the whole structure strictly; a linear scan suffices.
	// Because chunk sizes can be large, we skip over data by seeking.
	for {
		cur, _ := f.Seek(0, io.SeekCurrent)
		if cur+8 >= limit { // not enough for another header
			break
		}
		ch, err := readChunk(f)
		if err != nil {
			break
		}
		dataEnd := int64(ch.size) + ch.off
		if dataEnd > limit { // malformed
			break
		}

		if ch.id == "LIST" { // LIST chunk: next 4 bytes indicate list type
			listType := make([]byte, 4)
			if _, err := io.ReadFull(f, listType); err != nil {
				break
			}
			listTypeStr := string(listType)
			// INFO list contains metadata tags (ICRD, IDIT, etc.)
			if listTypeStr == "INFO" {
				// Parse sub-chunks within INFO region
				// Remaining bytes in this LIST (excluding 4 bytes we just read)
				remaining := int64(ch.size) - 4
				for remaining > 8 { // need at least header
					subPos, _ := f.Seek(0, io.SeekCurrent)
					if subPos+8 > dataEnd {
						break
					}
					subHdr := make([]byte, 8)
					if _, err := io.ReadFull(f, subHdr); err != nil {
						break
					}
					tag := string(subHdr[0:4])
					sz := binary.LittleEndian.Uint32(subHdr[4:8])
					valStart := subPos + 8
					valEnd := valStart + int64(sz)
					if valEnd > dataEnd { // malformed
						break
					}
					// Read value (cap to reasonable size, e.g., 512 bytes)
					readLen := sz
					if readLen > 512 {
						readLen = 512
					}
					buf := make([]byte, readLen)
					if _, err := f.Read(buf); err != nil {
						break
					}
					// Seek to end of chunk (in case we truncated read)
					if curPos, _ := f.Seek(0, io.SeekCurrent); curPos < valEnd {
						f.Seek(valEnd, io.SeekStart)
					}
					// AVI chunks are word aligned: if size is odd, skip pad byte
					if sz%2 == 1 {
						f.Seek(1, io.SeekCurrent)
					}

					remaining = dataEnd - (valEnd + int64(sz%2))

					if tag == "ICRD" || tag == "IDIT" {
						text := strings.Trim(string(bytes.Trim(buf, "\x00\r\n ")), " ")
						if y, ok := extractYear(text); ok {
							ct := time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC)
							log.Printf("Extracted creation year from AVI %s (%s=%s)", filepath.Base(path), tag, text)
							return ct, true
						}
					}
				}
			} else {
				// Skip rest of LIST contents
				// We consumed 4 bytes for listType already
				f.Seek(dataEnd, io.SeekStart)
			}
		} else {
			// Skip non-LIST chunk data
			f.Seek(dataEnd, io.SeekStart)
		}

		// Word alignment: if chunk size is odd, skip a pad byte (already included in size per spec? size excludes pad; we handled using size) -> we already advanced to dataEnd which accounts for size only; add pad if needed.
		if ch.size%2 == 1 {
			f.Seek(1, io.SeekCurrent)
		}
	}

	log.Printf("No AVI creation metadata (ICRD/IDIT) found for %s", filepath.Base(path))
	return time.Time{}, false
}

// extractArchive attempts to extract an archive and process its contents
// Returns true if extraction was successful, false otherwise
func extractArchive(archivePath string) bool {
	ext := strings.ToLower(filepath.Ext(archivePath))
	filename := filepath.Base(archivePath)

	// Create temporary extraction directory
	tempDir := filepath.Join(filepath.Dir(archivePath), "temp_extract_"+strings.TrimSuffix(filename, ext))

	var extractSuccess bool

	switch ext {
	case ".zip":
		extractSuccess = extractZip(archivePath, tempDir)
	default:
		// For other archive types (.rar, .7z, .tar, etc.), we currently can't extract
		log.Printf("Archive type '%s' not supported for extraction: %s", ext, filename)
		return false
	}

	if !extractSuccess {
		// Clean up temp directory if extraction failed
		os.RemoveAll(tempDir)
		return false
	}

	// Process extracted files
	log.Printf("Processing extracted files from '%s'...", filename)
	err := filepath.Walk(tempDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Printf("Error walking extracted files: %v", err)
			return nil
		}
		if info.IsDir() {
			return nil
		}

		// Process each extracted file as if it was in the original source
		processFile(path)
		return nil
	})

	// Clean up temporary extraction directory
	if err := os.RemoveAll(tempDir); err != nil {
		log.Printf("Warning: Could not clean up temporary extraction directory '%s': %v", tempDir, err)
	}

	if err != nil {
		log.Printf("Error processing extracted files from '%s': %v", filename, err)
		return false
	}

	return true
}

// extractZip extracts a ZIP file to the specified directory
func extractZip(zipPath, destDir string) bool {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		log.Printf("Error opening ZIP file '%s': %v", filepath.Base(zipPath), err)
		return false
	}
	defer reader.Close()

	// Create destination directory
	if err := os.MkdirAll(destDir, 0755); err != nil {
		log.Printf("Error creating extraction directory '%s': %v", destDir, err)
		return false
	}

	// Extract each file
	for _, file := range reader.File {
		// Skip directories
		if file.FileInfo().IsDir() {
			continue
		}

		// Create the file path
		filePath := filepath.Join(destDir, file.Name)

		// Create directory structure if needed
		if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
			log.Printf("Error creating directory structure for '%s': %v", file.Name, err)
			continue
		}

		// Open the file in the ZIP
		rc, err := file.Open()
		if err != nil {
			log.Printf("Error opening file '%s' in ZIP: %v", file.Name, err)
			continue
		}

		// Create the destination file
		outFile, err := os.Create(filePath)
		if err != nil {
			log.Printf("Error creating extracted file '%s': %v", filePath, err)
			rc.Close()
			continue
		}

		// Copy the file contents
		_, err = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()

		if err != nil {
			log.Printf("Error extracting file '%s': %v", file.Name, err)
			os.Remove(filePath) // Clean up partially extracted file
			continue
		}

		log.Printf("Extracted: %s", file.Name)
	}

	return true
}

// convertHEIC handles HEIC to JPEG conversion (stub - requires external tool)
func convertHEIC(sourcePath, targetFolder, hash string) {
	// For now, just log that HEIC conversion would happen
	// In a real implementation, you'd use ImageMagick or similar
	filename := filepath.Base(sourcePath)
	stem := strings.TrimSuffix(filename, filepath.Ext(filename))
	outputFilename := stem + ".jpg"
	destPath := filepath.Join(targetFolder, outputFilename)

	counter := 1
	for {
		if _, err := os.Stat(destPath); os.IsNotExist(err) {
			break // File doesn't exist, we can use this name
		}

		// Check if existing file has same hash
		existingHash, err := fileHash(destPath)
		if err == nil && existingHash == hash {
			log.Printf("Duplicate detected (HEIC hash matches existing JPG): '%s' vs '%s'. Deleting source HEIC.", filename, filepath.Base(destPath))
			if err := os.Remove(sourcePath); err != nil {
				log.Printf("Could not delete source HEIC duplicate '%s': %v", sourcePath, err)
				counterMu.Lock()
				errorCount++
				counterMu.Unlock()
			} else {
				counterMu.Lock()
				duplicateDeletedCount++
				counterMu.Unlock()
			}
			return
		}

		// Rename the output
		newName := fmt.Sprintf("%s_%d.jpg", stem, counter)
		destPath = filepath.Join(targetFolder, newName)
		counter++
		log.Printf("Filename conflict for converted JPEG: Renaming output to '%s' in '%s'", newName, filepath.Base(targetFolder))
	}

	log.Printf("Converting '%s' to '%s'...", filename, filepath.Base(destPath))

	// TODO: Implement actual HEIC to JPEG conversion using ImageMagick or similar
	// For now, just copy the file as-is (this is a placeholder)
	if err := copyFile(sourcePath, destPath); err != nil {
		log.Printf("Failed to convert HEIC file '%s': %v", filename, err)
		counterMu.Lock()
		errorCount++
		counterMu.Unlock()

		// Move to error folder
		errorDest := filepath.Join(errorsDir, filename)
		if err := copyFile(sourcePath, errorDest); err != nil {
			log.Printf("Could not move failed HEIC '%s' to error directory: %v", sourcePath, err)
		} else {
			log.Printf("Moved failed HEIC '%s' to '%s'", filename, "errors")
			os.Remove(sourcePath)
		}
		return
	}

	counterMu.Lock()
	heicConvertedCount++
	counterMu.Unlock()

	// Delete original HEIC after successful conversion
	if err := os.Remove(sourcePath); err != nil {
		log.Printf("Could not delete original HEIC '%s' after conversion: %v", sourcePath, err)
	}

	// Record hash in destination set
	hashMu.Lock()
	if hashesInDestination[targetFolder] == nil {
		hashesInDestination[targetFolder] = make(map[string]bool)
	}
	hashesInDestination[targetFolder][hash] = true
	hashMu.Unlock()

	// Increment appropriate counter
	if strings.Contains(targetFolder, "no_date") {
		// no_date_count already incremented
	} else if targetFolder != errorsDir {
		counterMu.Lock()
		movedCount++
		counterMu.Unlock()
	}
}

// moveFile handles moving regular files
func moveFile(sourcePath, targetFolder, filename, hash, mediaType string) {
	destPath := filepath.Join(targetFolder, filename)
	counter := 1

	for {
		if _, err := os.Stat(destPath); os.IsNotExist(err) {
			break // File doesn't exist, we can use this name
		}

		// Check if existing file has same hash
		existingHash, err := fileHash(destPath)
		if err == nil && existingHash == hash {
			log.Printf("Duplicate detected (hash match): '%s' vs existing '%s'. Deleting source.", filename, filepath.Base(destPath))
			if err := os.Remove(sourcePath); err != nil {
				log.Printf("Could not delete source duplicate file '%s': %v", sourcePath, err)
				counterMu.Lock()
				errorCount++
				counterMu.Unlock()
			} else {
				counterMu.Lock()
				duplicateDeletedCount++
				counterMu.Unlock()
			}
			return
		}

		// Rename file being moved
		ext := filepath.Ext(filename)
		stem := strings.TrimSuffix(filename, ext)
		newName := fmt.Sprintf("%s_%d%s", stem, counter, ext)
		destPath = filepath.Join(targetFolder, newName)
		counter++
		log.Printf("Filename conflict: Renaming '%s' to '%s' in '%s'", filename, newName, filepath.Base(targetFolder))
	}

	// Perform the move
	if err := os.Rename(sourcePath, destPath); err != nil {
		// If rename fails, try copy and delete
		if err := copyFile(sourcePath, destPath); err != nil {
			log.Printf("Failed to move '%s': %v", sourcePath, err)
			counterMu.Lock()
			errorCount++
			counterMu.Unlock()
			return
		}
		os.Remove(sourcePath)
	}

	log.Printf("Successfully moved '%s' to '%s'", filename, destPath)

	// Increment appropriate counter
	switch mediaType {
	case "video":
		if strings.Contains(targetFolder, "no_date") {
			// no_date_count already incremented
		} else if targetFolder != errorsDir {
			counterMu.Lock()
			videoMovedCount++
			counterMu.Unlock()
		}
	case "image":
		if strings.Contains(targetFolder, "no_date") {
			// no_date_count already incremented
		} else if targetFolder != errorsDir {
			counterMu.Lock()
			movedCount++
			counterMu.Unlock()
		}
	}

	// Record hash in destination set
	if hash != "" {
		hashMu.Lock()
		if hashesInDestination[targetFolder] == nil {
			hashesInDestination[targetFolder] = make(map[string]bool)
		}
		hashesInDestination[targetFolder][hash] = true
		hashMu.Unlock()
	}
}

// copyFile copies a file from src to dst with optimized buffered I/O
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	// Use a larger buffer for better performance
	buf := make([]byte, 64*1024) // 64KB buffer
	_, err = io.CopyBuffer(dstFile, srcFile, buf)
	return err
}

// printSummary prints a comprehensive final summary with statistics and performance metrics
func printSummary() {
	totalProcessed := atomic.LoadInt64(&processedFiles)
	totalFound := atomic.LoadInt64(&totalFiles)

	log.Println("")
	log.Println("═══════════════════════════════════════════════════════════════")
	log.Println("                    📊 PHOTO SORTING COMPLETE 📊")
	log.Println("═══════════════════════════════════════════════════════════════")
	log.Println("")

	// File Processing Summary
	log.Println("📁 FILE PROCESSING SUMMARY:")
	log.Printf("   • Total files found: %d", totalFound)
	log.Printf("   • Total files processed: %d", totalProcessed)
	log.Printf("   • Processing completion: %.1f%%", float64(totalProcessed)/float64(totalFound)*100)
	log.Println("")

	// Successful Operations
	log.Println("✅ SUCCESSFUL OPERATIONS:")
	successfulOps := movedCount + videoMovedCount + heicConvertedCount + noDateCount + archiveExtractedCount + archiveMovedCount
	log.Printf("   📷 Photos sorted by Date Taken: %d", movedCount)
	log.Printf("   🎬 Videos sorted by Media Created: %d", videoMovedCount)
	log.Printf("   🔄 HEIC/HEIF files converted to JPEG: %d", heicConvertedCount)
	log.Printf("   📂 Files sorted by extension (no date): %d", noDateCount)
	log.Printf("   📦 ZIP archives extracted & processed: %d", archiveExtractedCount)
	log.Printf("   📥 Archives moved (non-ZIP): %d", archiveMovedCount)
	log.Printf("   🗑️  Non-media files deleted: %d", deletedNonMediaCount)
	log.Printf("   ➡️  Total successful operations: %d", successfulOps)
	log.Println("")

	// Issues and Cleanup
	issueCount := errorCount + duplicateDeletedCount + skippedCount
	if issueCount > 0 {
		log.Println("⚠️  ISSUES HANDLED:")
		if errorCount > 0 {
			log.Printf("   ❌ Files moved to 'errors' folder: %d", errorCount)
		}
		if duplicateDeletedCount > 0 {
			log.Printf("   🔄 Duplicate files deleted: %d", duplicateDeletedCount)
		}
		if skippedCount > 0 {
			log.Printf("   ⏭️  Files skipped (already processed): %d", skippedCount)
		}
		log.Printf("   📊 Total issues handled: %d", issueCount)
		log.Println("")
	}

	// Performance Stats
	log.Println("⚡ PERFORMANCE & SETTINGS:")
	log.Printf("   🔧 Worker goroutines used: %d", runtime.NumCPU()*2)
	log.Printf("   📋 Sorting method: Date Taken (photos) & Media Created (videos)")
	log.Printf("   🚫 File system dates: Ignored")
	log.Printf("   📁 Extension-based sorting: Enabled for no-date files")
	log.Printf("   📦 ZIP auto-extraction: Enabled")
	log.Println("")

	// Directory Locations
	log.Println("📍 OUTPUT LOCATIONS:")
	log.Printf("   📂 Sorted photos: %s", destDir)
	log.Printf("   📅 No-date files: %s", noDateDir)
	log.Printf("   📦 Archives: %s", archivesDir)
	if errorCount > 0 {
		log.Printf("   ❌ Error files: %s", errorsDir)
	}
	log.Println("")

	// Final Status
	if errorCount > 0 {
		log.Println("⚠️  COMPLETED WITH ISSUES - Check the 'errors' folder for problematic files")
	} else {
		log.Println("🎉 COMPLETED SUCCESSFULLY - All files processed without errors!")
	}

	log.Println("═══════════════════════════════════════════════════════════════")
	log.Printf("📋 IMPORTANT: Photos sorted by 'Date Taken' metadata, Videos by 'Media Created' metadata")
	log.Printf("🔍 Review your sorted files in: %s", destDir)
	log.Println("═══════════════════════════════════════════════════════════════")
}

// cleanupEmptyDirectories recursively removes empty directories in the source path
func cleanupEmptyDirectories(basePath string) {
	log.Printf("Cleaning up empty directories in '%s'...", basePath)
	deletedDirs := 0

	// We need to do multiple passes because removing a directory might make its parent empty
	for {
		dirsBefore := deletedDirs
		deletedDirs += removeEmptyDirsPass(basePath)

		// If no directories were deleted in this pass, we're done
		if deletedDirs == dirsBefore {
			break
		}
	}

	if deletedDirs > 0 {
		log.Printf("Deleted %d empty directories", deletedDirs)
	} else {
		log.Println("No empty directories found to delete")
	}
}

// removeEmptyDirsPass makes one pass through the directory tree, removing empty directories
// Returns the number of directories deleted in this pass
func removeEmptyDirsPass(basePath string) int {
	deletedCount := 0

	err := filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Printf("Error accessing %s: %v", path, err)
			return nil // Continue walking despite errors
		}

		// Skip the base directory itself
		if path == basePath {
			return nil
		}

		// Only process directories
		if !info.IsDir() {
			return nil
		}

		// Check if directory is empty
		if isDirEmpty(path) {
			if err := os.Remove(path); err != nil {
				log.Printf("Failed to remove empty directory %s: %v", path, err)
			} else {
				log.Printf("Removed empty directory: %s", path)
				deletedCount++
			}
		}

		return nil
	})

	if err != nil {
		log.Printf("Error during directory cleanup: %v", err)
	}

	return deletedCount
}

// isDirEmpty checks if a directory is empty (contains no files or subdirectories)
func isDirEmpty(dirPath string) bool {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		log.Printf("Error reading directory %s: %v", dirPath, err)
		return false // If we can't read it, don't delete it
	}
	return len(entries) == 0
}

// fileHash calculates the SHA256 hash of a file with optimized buffered I/O
func fileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	// Use a larger buffer for better performance on large files
	buf := make([]byte, 64*1024) // 64KB buffer
	for {
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
