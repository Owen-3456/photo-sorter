# Photo Sorter

A Go application to automatically sort photos and videos from an input directory into an output directory structured by year. It handles various media formats, converts HEIC/HEIF files to JPEG, and detects duplicates with concurrent processing.

<span style="color: red; font-weight: bold;">⚠️ WARNING: This application modifies and deletes files. Use with caution and back up your data before running.</span>

## Features

*   **Concurrent Processing:** Uses multiple goroutines (4 workers) for faster file processing.
*   **Year-based Sorting:** Sorts images based on EXIF 'Date Taken' metadata (year) into `sorted_photos/YYYY` folders.
*   **Size-based Categorization:** Places videos and images without valid EXIF date into `no_date` subfolders organized by file size.
*   **Multiple File Types:** Supports common image formats (JPG, JPEG, PNG, GIF, TIFF, BMP, HEIC, HEIF) and video formats (MP4, AVI, MOV, WMV, MKV, FLV, MPEG, MPG, M4V).
*   **Archive Handling:** Moves archive files (ZIP, RAR, 7Z, TAR, etc.) to a dedicated `archives` folder.
*   **HEIC/HEIF Support:** Converts `.heic` and `.heif` files to JPEG format (currently placeholder - requires external tool like ImageMagick).
*   **Duplicate Detection:** Calculates SHA256 hashes to identify and handle duplicate files. Duplicates are deleted from source.
*   **Error Handling:** Moves files that cause processing errors to an `errors` folder.
*   **Non-Media Files:** Deletes files that are not recognized as supported media or archive types.
*   **Empty Directory Cleanup:** Automatically removes empty directories from the source after processing.
*   **Comprehensive Logging:** Provides detailed logs about the sorting process with timestamps.

## Usage

1.  **Place Files:** Put all the photos and videos you want to sort into the `unsorted_photos` directory (create this folder in the same location as the executable). You can have subdirectories within `unsorted_photos`; the application will scan recursively.
2.  **Run the program** Download the latest release from the [Releases](github.com/Owen-3456/photo-sorter/releases) page.
3.  **Check Errors:** Check the console output and the `errors` folder for any issues.
4. **Check Archives:** If you want to sort files from archives, extract them manually into the `unsorted_photos` folder and rerun the program.

## Directory Structure

After running, the following structure is created:
```
photo-sorter.exe    # Executable
unsorted_photos/    # Input directory (user-provided)
sorted_photos/
├── 2023/           # Images with EXIF year 2023
├── 2024/           # Images with EXIF year 2024
├── no_date/        # Files without EXIF date, organized by size:
│   ├── tiny_under_0.5MB/
│   ├── small_0.5-1MB/
│   ├── medium_1-2MB/
│   ├── large_2-5MB/
│   ├── xlarge_5-10MB/
│   └── huge_over_10MB/
├── archives/       # ZIP, RAR, 7Z and other archive files
└── errors/         # Files that caused processing errors
```
