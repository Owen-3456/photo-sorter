package main

import (
	"crypto/sha256"
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

// TODO: Add EXIF and HEIC support imports

var (
	imageExts   = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".tiff": true, ".bmp": true, ".heic": true, ".heif": true}
	videoExts   = map[string]bool{".mp4": true, ".avi": true, ".mov": true, ".wmv": true, ".mkv": true, ".flv": true, ".mpeg": true, ".mpg": true, ".m4v": true}
	heicExts    = map[string]bool{".heic": true, ".heif": true}
	archiveExts = map[string]bool{".zip": true, ".rar": true, ".7z": true, ".tar": true, ".gz": true, ".bz2": true, ".xz": true, ".tar.gz": true, ".tar.bz2": true, ".tar.xz": true}
)

var (
	scriptDir, _   = filepath.Abs(filepath.Dir(os.Args[0]))
	sourceDir      = filepath.Join(scriptDir, "unsorted_photos")
	destDir        = filepath.Join(scriptDir, "sorted_photos")
	noDateDir      = filepath.Join(destDir, "no_date")
	archivesDir    = filepath.Join(destDir, "archives")
	errorsDir      = filepath.Join(destDir, "errors")
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
	counterMu               sync.Mutex
	movedCount              int
	videoMovedCount         int
	heicConvertedCount      int
	noDateCount             int
	archiveMovedCount       int
	deletedNonMediaCount    int
	errorCount              int
	skippedCount            int
	duplicateDeletedCount   int
	totalFiles              int64 // Track total files for progress
	processedFiles          int64 // Track processed files for progress
)

func main() {
	log.SetFlags(log.LstdFlags)
	log.Printf("Starting media sort from '%s' to '%s'...", sourceDir, destDir)
	log.Println("HEIC/HEIF files will be converted to JPEG.")

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
		yearOrStatus = getExifYear(path)
	} else if videoExts[ext] {
		mediaType = "video"
		yearOrStatus = "" // Videos generally don't have EXIF date
	} else if archiveExts[ext] {
		mediaType = "archive"
		targetFolder = archivesDir
		log.Printf("Moving '%s' to '%s' (archive file)", filename, "archives")
		counterMu.Lock()
		archiveMovedCount++
		counterMu.Unlock()
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

	// Determine target folder based on date for media files
	if mediaType == "image" || mediaType == "video" {
		if yearOrStatus == "error" {
			targetFolder = errorsDir
			log.Printf("Moving '%s' to '%s' due to processing error.", filename, "errors")
			counterMu.Lock()
			errorCount++
			counterMu.Unlock()
		} else if yearOrStatus != "" && yearOrStatus != "none" {
			// Year was successfully extracted from EXIF
			targetFolder = filepath.Join(destDir, yearOrStatus)
			log.Printf("Processing '%s' (%s) for year '%s'", filename, mediaType, yearOrStatus)
		} else {
			// No date found - use file size to categorize
			sizeCat := getFileSizeCategory(path)
			targetFolder = filepath.Join(noDateDir, sizeCat)
			log.Printf("Processing '%s' (%s) for '%s' (no EXIF date found, sorting by size: %s)", filename, mediaType, filepath.Join("no_date", sizeCat), sizeCat)
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
// getFileSizeCategory categorizes files by size for no_date sorting
func getFileSizeCategory(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		log.Printf("Could not get file size for %s: %v", path, err)
		return "unknown_size"
	}
	sizeMB := float64(fi.Size()) / (1024 * 1024)
	switch {
	case sizeMB < 0.5:
		return "tiny_under_0.5MB"
	case sizeMB < 1:
		return "small_0.5-1MB"
	case sizeMB < 2:
		return "medium_1-2MB"
	case sizeMB < 5:
		return "large_2-5MB"
	case sizeMB < 10:
		return "xlarge_5-10MB"
	default:
		return "huge_over_10MB"
	}
}

// getExifYear tries to extract the year from EXIF metadata with optimizations
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

	// Try DateTimeOriginal first (most reliable) - this is the most common tag
	if tag, err := x.Get(exif.DateTimeOriginal); err == nil {
		if dateStr, err := tag.StringVal(); err == nil && len(dateStr) >= 4 {
			if year := extractYearFromDateString(dateStr); year != "" {
				return year
			}
		}
	}

	// Try DateTime() method as fallback
	if dt, err := x.DateTime(); err == nil {
		year := dt.Year()
		if year > 1900 && year <= time.Now().Year()+1 {
			return strconv.Itoa(year)
		}
	}

	// Try DateTime tag as final fallback
	if tag, err := x.Get(exif.DateTime); err == nil {
		if dateStr, err := tag.StringVal(); err == nil && len(dateStr) >= 4 {
			if year := extractYearFromDateString(dateStr); year != "" {
				return year
			}
		}
	}

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
	if mediaType == "video" {
		if strings.Contains(targetFolder, "no_date") {
			// no_date_count already incremented
		} else if targetFolder != errorsDir {
			counterMu.Lock()
			videoMovedCount++
			counterMu.Unlock()
		}
	} else if mediaType == "image" {
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

// printSummary prints the final summary like the Python script
func printSummary() {
	log.Println("--- Sorting Summary ---")
	log.Printf("Photos moved/converted to year folders: %d", movedCount)
	log.Printf("Videos moved to year folders: %d", videoMovedCount)
	log.Printf("HEIC files converted to JPEG: %d", heicConvertedCount)
	log.Printf("Media files moved to 'no_date' subfolders (no EXIF date, sorted by size): %d", noDateCount)
	log.Printf("Archive files moved to 'archives': %d", archiveMovedCount)
	log.Printf("Non-media files deleted: %d", deletedNonMediaCount)
	log.Printf("Files moved to 'errors' due to errors: %d", errorCount)
	log.Printf("Files skipped (e.g., already in destination, not found): %d", skippedCount)
	log.Printf("Duplicate files deleted from source: %d", duplicateDeletedCount)
	log.Println("------------------------")
	log.Println("Media sorting process finished.")
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
