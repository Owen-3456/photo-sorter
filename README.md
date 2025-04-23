# Photo Sorter

A Python script to automatically sort photos and videos from an input directory into an output directory structured by year. It handles various media formats, converts HEIC/HEIF files to JPEG, and detects duplicates.

## Features

*   Sorts images based on EXIF 'Date Taken' metadata (year).
*   Places videos and images without valid EXIF date into a `no_date` folder.
*   Supports common image formats (JPG, PNG, GIF, TIFF, BMP) and video formats (MP4, AVI, MOV, WMV, MKV, etc.).
*   **HEIC/HEIF Support:** Automatically converts `.heic` and `.heif` files to JPEG format using `pillow-heif`.
*   **Duplicate Detection:** Calculates SHA256 hashes to identify and handle duplicate files within the destination folders during a single run. Duplicates found in the source are deleted.
*   **Error Handling:** Moves files that cause processing errors (e.g., corrupted images, permission issues) to an `errors` folder.
*   **Non-Media Files:** Moves files that are not recognized as supported image or video types to a `not_photos` folder.
*   **Logging:** Provides informative logs about the sorting process, including moved files, conversions, duplicates, and errors.

## Requirements

*   Python 3.x
*   Pillow
*   Pillow-HEIF

Install the required packages using pip:

```shell
pip install Pillow pillow-heif
```

## Usage

1.  **Place Files:** Put all the photos and videos you want to sort into the `unsorted_photos` directory. You can have subdirectories within `unsorted_photos`; the script will scan recursively.
2.  **Run Script:** Execute the script from the command line within the directory above `unsorted_photos`.
3.  **Check Output:** The sorted files will be organized in the `sorted_photos` directory. Check the console output and the `errors` folder for any issues.

## How it Works

1.  **Scan:** The script walks through the `unsorted_photos` directory.
2.  **Identify:** It checks the file extension to determine if it's a known image or video type.
3.  **Get Date (Images):** For images, it attempts to read EXIF metadata (specifically `DateTimeOriginal` or `DateTime`) to find the year the photo was taken.
4.  **Determine Target:**
    *   If a year is found, the target is `sorted_photos/YYYY`.
    *   If no date is found (or it's a video), the target is `sorted_photos/no_date`.
    *   If it's not a recognized media file, the target is `sorted_photos/not_photos`.
    *   If an error occurs during processing (e.g., reading EXIF, hashing), the target becomes `sorted_photos/errors`.
5.  **Handle HEIC:** If the file is HEIC/HEIF, it converts it to JPEG before moving. The original HEIC is deleted after successful conversion.
6.  **Check Duplicates:** Before moving or converting, it calculates the file's hash.
    *   It checks if a file with the same hash already exists *in the target folder* (based on files processed *during the current run*). If so, the source file is deleted.
    *   It checks if a file with the *same name* already exists in the target folder. If the hashes match, the source file is deleted (duplicate). If the hashes differ, the file being moved is renamed (e.g., `image_1.jpg`).
7.  **Move:** The file (or its converted version) is moved to the determined target folder.
8.  **Log:** Actions, warnings, and errors are logged to the console.

## Notes

*   The script **moves** files from `unsorted_photos`. Ensure you have backups if needed.
*   Duplicate detection compares the hash of the source file with files already present *in the specific target folder* during the current script execution. It also handles filename conflicts by renaming.
*   Video files are typically placed in `no_date` as they often lack standardized 'Date Taken' metadata accessible via Pillow.
*   The quality for HEIC to JPEG conversion is set to 95 (can be adjusted in `main.py`).

