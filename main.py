import os
import shutil
from pathlib import Path
from PIL import Image
from PIL.ExifTags import TAGS
import logging
import hashlib
import datetime  # Added for file modification time
from pillow_heif import register_heif_opener  # Added for HEIC support

# Register HEIF opener with Pillow
register_heif_opener()

# Import UnidentifiedImageError globally and define a dummy if it doesn't exist
try:
    from PIL import UnidentifiedImageError
except ImportError:
    # Define a dummy exception if Pillow version is older
    class UnidentifiedImageError(Exception):
        pass


# Setup logging
logging.basicConfig(
    level=logging.INFO, format="%(asctime)s - %(levelname)s - %(message)s"
)

# Define base paths relative to the script location
SCRIPT_DIR = Path(__file__).parent
SOURCE_DIR = SCRIPT_DIR / "unsorted_photos"
DEST_DIR = SCRIPT_DIR / "sorted_photos"
NO_DATE_DIR = DEST_DIR / "no_date"
ARCHIVES_DIR = DEST_DIR / "archives"  # Folder for archive files
ERROR_DIR = DEST_DIR / "errors"  # Folder for files that cause processing errors
CONVERTED_DIR = (
    DEST_DIR / "converted_originals"
)  # Optional: Folder to keep original HEICs

# Common image file extensions (case-insensitive)
IMAGE_EXTENSIONS = {".jpg", ".jpeg", ".png", ".gif", ".tiff", ".bmp", ".heic", ".heif"}
# Common video file extensions (case-insensitive)
VIDEO_EXTENSIONS = {
    ".mp4",
    ".avi",
    ".mov",
    ".wmv",
    ".mkv",
    ".flv",
    ".mpeg",
    ".mpg",
    ".m4v",
}
# HEIC/HEIF extensions specifically
HEIC_EXTENSIONS = {".heic", ".heif"}
# Common archive file extensions (case-insensitive)
ARCHIVE_EXTENSIONS = {
    ".zip",
    ".rar",
    ".7z",
    ".tar",
    ".gz",
    ".bz2",
    ".xz",
    ".tar.gz",
    ".tar.bz2",
    ".tar.xz",
}


def get_file_size_category(file_path):
    """
    Categorizes files by size to create more evenly distributed subfolders.
    Returns a string category based on file size.
    """
    try:
        size_bytes = file_path.stat().st_size
        size_mb = size_bytes / (1024 * 1024)

        if size_mb < 0.5:
            return "tiny_under_0.5MB"
        elif size_mb < 1:
            return "small_0.5-1MB"
        elif size_mb < 2:
            return "medium_1-2MB"
        elif size_mb < 5:
            return "large_2-5MB"
        elif size_mb < 10:
            return "xlarge_5-10MB"
        else:
            return "huge_over_10MB"
    except Exception as e:
        logging.warning(f"Could not get file size for {file_path}: {e}")
        return "unknown_size"


def get_date_taken(file_path):
    """
    Tries to extract the 'Date Taken' metadata (DateTimeOriginal or DateTime)
    from an image file. Returns the year as a string if found, otherwise None.
    """
    file_ext = file_path.suffix.lower()
    year = None

    # Only attempt EXIF for known image types
    if file_ext in IMAGE_EXTENSIONS:
        try:
            img = Image.open(file_path)
            # Ensure image data is loaded to read EXIF, especially for formats like HEIC
            img.load()
            exif_data = img._getexif()

            if exif_data:
                # Tag ID 36867 corresponds to DateTimeOriginal
                date_str = exif_data.get(36867)
                if date_str and isinstance(date_str, str) and len(date_str) >= 10:
                    if date_str[4] == ":" and date_str[7] == ":":
                        year = date_str[:4]  # Extract the year

                # Fallback to DateTime tag (306) if DateTimeOriginal is not present or invalid
                if not year:
                    date_str = exif_data.get(306)
                    if date_str and isinstance(date_str, str) and len(date_str) >= 10:
                        if date_str[4] == ":" and date_str[7] == ":":
                            year = date_str[:4]  # Extract the year

            # Close the image file explicitly after reading EXIF
            img.close()

            if year:
                return year  # Return year from EXIF if found

        except FileNotFoundError:
            logging.warning(f"File not found during EXIF read: {file_path}")
            return "error"  # Treat as error if file vanishes during check
        except UnidentifiedImageError:
            if (
                file_ext in IMAGE_EXTENSIONS
            ):  # Only log warning for expected image types
                logging.warning(
                    f"Cannot identify image file (possibly corrupt or not an image): {file_path}. Moving to no_date."
                )
            # Fall through to return None (no date found)
        except Exception as e:
            logging.error(f"Error reading EXIF data for {file_path}: {e}")
            # Fall through to return None (no date found)

    # If it's not an image or EXIF reading failed/didn't find a date
    return None  # Explicitly return None if no valid EXIF date was found


def calculate_hash(file_path, block_size=65536):
    """Calculates the SHA256 hash of a file."""
    sha256 = hashlib.sha256()
    try:
        with open(file_path, "rb") as f:
            while True:
                data = f.read(block_size)
                if not data:
                    break
                sha256.update(data)
        return sha256.hexdigest()
    except IOError as e:
        logging.error(f"Could not read file {file_path} to calculate hash: {e}")
        return None
    except Exception as e:  # Catch any other unexpected errors during hashing
        logging.error(f"Unexpected error hashing file {file_path}: {e}")
        return None


def sort_photos():
    """Sorts photos and videos from SOURCE_DIR to DEST_DIR based on year taken. Converts HEIC."""
    # Create destination directories if they don't exist
    DEST_DIR.mkdir(exist_ok=True)
    NO_DATE_DIR.mkdir(exist_ok=True)
    ARCHIVES_DIR.mkdir(exist_ok=True)
    ERROR_DIR.mkdir(exist_ok=True)
    # Optional: Create dir for original HEICs if you want to keep them
    # CONVERTED_DIR.mkdir(exist_ok=True)

    logging.info(f"Starting media sort from '{SOURCE_DIR}' to '{DEST_DIR}'...")
    logging.info(f"HEIC/HEIF files will be converted to JPEG.")

    moved_count = 0
    video_moved_count = 0
    heic_converted_count = 0
    no_date_count = 0
    archive_moved_count = 0  # Counter for archive files
    deleted_non_media_count = 0  # Changed from not_media_count
    error_count = 0
    skipped_count = 0
    duplicate_deleted_count = 0
    hashes_in_destination = {}

    # Check if source directory exists
    if not SOURCE_DIR.is_dir():
        logging.error(f"Source directory '{SOURCE_DIR}' not found. Exiting.")
        return

    for root, _, files in os.walk(SOURCE_DIR):
        current_source_dir = Path(root)
        for filename in files:
            source_path = current_source_dir / filename
            dest_path_final = None
            target_folder = None
            current_file_hash = None

            try:
                # Skip files that might already be in a destination structure
                if DEST_DIR in source_path.parents:
                    logging.info(
                        f"Skipping file already in destination structure: {source_path}"
                    )
                    skipped_count += 1
                    continue

                file_ext = source_path.suffix.lower()

                # Determine media type and get date
                year_or_status = None
                media_type = "unknown"

                if file_ext in IMAGE_EXTENSIONS:
                    media_type = "image"
                    year_or_status = get_date_taken(source_path)
                elif file_ext in VIDEO_EXTENSIONS:
                    media_type = "video"
                    # Videos generally don't have EXIF date, treat as no_date unless specific metadata exists (not implemented here)
                    year_or_status = None  # Assume no date for videos
                elif file_ext in ARCHIVE_EXTENSIONS:
                    media_type = "archive"
                    target_folder = ARCHIVES_DIR
                    archive_moved_count += 1
                    logging.info(
                        f"Moving '{source_path.name}' to '{target_folder.name}' (archive file)"
                    )
                else:
                    media_type = "other"
                    # Delete non-media files
                    try:
                        os.remove(source_path)
                        deleted_non_media_count += 1
                        logging.info(
                            f"Deleted '{source_path.name}' (not a recognized media file)"
                        )
                        continue  # Skip to next file
                    except OSError as e:
                        logging.error(
                            f"Could not delete non-media file '{source_path}': {e}"
                        )
                        error_count += 1
                        continue  # Skip to next file

                # Determine target folder based on date for media files
                if media_type in ["image", "video"]:
                    if year_or_status == "error":
                        target_folder = ERROR_DIR
                        error_count += 1
                        logging.warning(
                            f"Moving '{source_path.name}' to '{target_folder.name}' due to processing error."
                        )
                    elif year_or_status:  # Year was successfully extracted from EXIF
                        target_folder = DEST_DIR / year_or_status
                        logging.info(
                            f"Processing '{source_path.name}' ({media_type}) for year '{year_or_status}'"
                        )
                    else:  # No date found (None returned from get_date_taken or it's a video)
                        # Use file size to categorize no_date files into subfolders
                        size_category = get_file_size_category(source_path)
                        target_folder = NO_DATE_DIR / size_category
                        no_date_count += 1  # Increment here based on determination
                        logging.info(
                            f"Processing '{source_path.name}' ({media_type}) for '{target_folder.name}' (no EXIF date found, sorting by size: {size_category})"
                        )

                # --- Action: Convert HEIC, Move Video/Other Image, or Move Non-Media ---
                if target_folder:
                    target_folder.mkdir(exist_ok=True, parents=True)

                    # Calculate hash of the original file for duplicate check
                    current_file_hash = calculate_hash(source_path)

                    if current_file_hash is None:
                        logging.warning(
                            f"Could not calculate hash for {source_path.name}. Moving to errors folder."
                        )
                        target_folder = ERROR_DIR
                        target_folder.mkdir(exist_ok=True, parents=True)
                        error_count += 1
                        # Fall through to move logic, target is now ERROR_DIR
                    else:
                        # Check for duplicates based on hash in the target folder for this run
                        target_hashes = hashes_in_destination.setdefault(
                            str(target_folder), set()
                        )
                        if current_file_hash in target_hashes:
                            logging.warning(
                                f"Duplicate detected (hash match in run): '{source_path.name}' for '{target_folder.name}'. Deleting source."
                            )
                            try:
                                os.remove(source_path)
                                duplicate_deleted_count += 1
                            except OSError as e:
                                logging.error(
                                    f"Could not delete duplicate source file '{source_path}': {e}"
                                )
                                error_count += 1
                            continue  # Skip processing this file

                    # --- Specific HEIC Conversion Logic ---
                    if media_type == "image" and file_ext in HEIC_EXTENSIONS:
                        output_filename = source_path.with_suffix(".jpg").name
                        dest_path_final = target_folder / output_filename
                        counter = 1

                        # Check for filename conflict for the *output JPEG*
                        while dest_path_final.exists():
                            existing_file_hash = calculate_hash(dest_path_final)
                            # If existing JPG has same hash as *original* HEIC, treat HEIC as duplicate
                            if (
                                existing_file_hash
                                and current_file_hash == existing_file_hash
                            ):
                                logging.warning(
                                    f"Duplicate detected (HEIC hash matches existing JPG): '{source_path.name}' vs '{dest_path_final.name}'. Deleting source HEIC."
                                )
                                try:
                                    os.remove(source_path)
                                    duplicate_deleted_count += 1
                                except OSError as e:
                                    logging.error(
                                        f"Could not delete source HEIC duplicate '{source_path}': {e}"
                                    )
                                    error_count += 1
                                dest_path_final = None  # Signal to skip conversion/move
                                break  # Exit filename conflict loop
                            else:
                                # Rename the output JPEG
                                new_name = f"{source_path.stem}_{counter}.jpg"
                                dest_path_final = target_folder / new_name
                                counter += 1
                                logging.warning(
                                    f"Filename conflict for converted JPEG: Renaming output to '{new_name}' in '{target_folder.name}'"
                                )

                        if dest_path_final is None:  # Was duplicate of existing file
                            continue  # Skip to next file

                        # Perform conversion
                        try:
                            logging.info(
                                f"Converting '{source_path.name}' to '{dest_path_final.name}'..."
                            )
                            with Image.open(source_path) as img:
                                img.save(
                                    dest_path_final, "JPEG", quality=95
                                )  # Adjust quality as needed
                            heic_converted_count += 1

                            # Optional: Move original HEIC instead of deleting
                            # shutil.move(str(source_path), str(CONVERTED_DIR / source_path.name))
                            # logging.info(f"Moved original HEIC '{source_path.name}' to '{CONVERTED_DIR.name}'")

                            # Delete original HEIC after successful conversion
                            try:
                                os.remove(source_path)
                            except OSError as e:
                                logging.error(
                                    f"Could not delete original HEIC '{source_path}' after conversion: {e}"
                                )
                                # Don't count as error, but log it. The JPG is there.

                            # Record hash of *original* HEIC in destination set
                            hashes_in_destination.setdefault(
                                str(target_folder), set()
                            ).add(current_file_hash)

                            # Increment move counts based on target folder
                            if str(target_folder).startswith(str(NO_DATE_DIR)):
                                # no_date_count already incremented earlier
                                pass
                            elif target_folder != ERROR_DIR:
                                moved_count += 1  # Count as photo move

                        except Exception as convert_e:
                            logging.error(
                                f"Failed to convert HEIC file '{source_path.name}': {convert_e}"
                            )
                            error_count += 1
                            # Attempt to move original HEIC to error folder
                            try:
                                error_dest = (
                                    ERROR_DIR / source_path.name
                                )  # Keep original name
                                # Add renaming logic for error folder if needed
                                shutil.move(str(source_path), str(error_dest))
                                logging.info(
                                    f"Moved failed HEIC '{source_path.name}' to '{ERROR_DIR.name}'"
                                )
                            except Exception as move_e:
                                logging.error(
                                    f"Could not move failed HEIC '{source_path}' to error directory: {move_e}"
                                )
                        continue  # Skip generic move logic

                    # --- Generic Move Logic (Videos, Other Images, Non-Media, Errors) ---
                    dest_path_final = target_folder / filename
                    counter = 1
                    while dest_path_final.exists():
                        existing_file_hash = calculate_hash(dest_path_final)
                        if (
                            existing_file_hash
                            and current_file_hash == existing_file_hash
                        ):
                            logging.warning(
                                f"Duplicate detected (hash match): '{source_path.name}' vs existing '{dest_path_final.name}'. Deleting source."
                            )
                            try:
                                os.remove(source_path)
                                duplicate_deleted_count += 1
                            except OSError as e:
                                logging.error(
                                    f"Could not delete source duplicate file '{source_path}': {e}"
                                )
                                error_count += 1
                            dest_path_final = None  # Signal to skip move
                            break  # Exit filename conflict loop
                        else:
                            # Rename file being moved
                            new_name = (
                                f"{source_path.stem}_{counter}{source_path.suffix}"
                            )
                            dest_path_final = target_folder / new_name
                            counter += 1
                            logging.warning(
                                f"Filename conflict: Renaming '{filename}' to '{new_name}' in '{target_folder.name}'"
                            )

                    if dest_path_final is None:  # Was duplicate of existing file
                        continue  # Skip to next file

                    # Perform the move
                    shutil.move(str(source_path), str(dest_path_final))
                    logging.info(
                        f"Successfully moved '{source_path.name}' to '{dest_path_final}'"
                    )

                    # Increment appropriate counter after successful move
                    if media_type == "video":
                        if str(target_folder).startswith(str(NO_DATE_DIR)):
                            # no_date_count already incremented earlier
                            pass
                        elif target_folder != ERROR_DIR:
                            video_moved_count += 1
                    elif media_type == "image":  # Non-HEIC images
                        if str(target_folder).startswith(str(NO_DATE_DIR)):
                            # no_date_count already incremented earlier
                            pass
                        elif target_folder != ERROR_DIR:
                            moved_count += 1
                    # non-media and error counts incremented earlier or during hash check failure

                    # Record hash in destination set
                    if current_file_hash:
                        hashes_in_destination.setdefault(str(target_folder), set()).add(
                            current_file_hash
                        )

            except FileNotFoundError:
                logging.warning(
                    f"File not found during move operation: '{source_path}' (maybe moved already?)"
                )
                skipped_count += 1
            except PermissionError as e:  # Added error detail
                logging.error(
                    f"Permission denied for '{source_path}' or its destination '{dest_path_final}'. Skipping."
                )
                error_count += 1
            except Exception as e:
                logging.error(f"Unexpected error processing file '{source_path}': {e}")
                error_count += 1
                # Attempt to move the problematic file to the error directory if it still exists
                if source_path.exists():
                    error_dest = ERROR_DIR / filename
                    counter = 1
                    while error_dest.exists():
                        new_name = f"{source_path.stem}_{counter}{source_path.suffix}"
                        error_dest = ERROR_DIR / new_name
                        counter += 1
                    shutil.move(str(source_path), str(error_dest))
                    logging.info(
                        f"Moved problematic file '{source_path.name}' to '{ERROR_DIR.name}'"
                    )

    logging.info("--- Sorting Summary ---")
    logging.info(f"Photos moved/converted to year folders: {moved_count}")
    logging.info(f"Videos moved to year folders: {video_moved_count}")
    logging.info(f"HEIC files converted to JPEG: {heic_converted_count}")
    logging.info(
        f"Media files moved to '{NO_DATE_DIR.name}' subfolders (no EXIF date, sorted by size): {no_date_count}"
    )  # Updated log message
    logging.info(f"Archive files moved to '{ARCHIVES_DIR.name}': {archive_moved_count}")
    logging.info(f"Non-media files deleted: {deleted_non_media_count}")
    logging.info(f"Files moved to '{ERROR_DIR.name}' due to errors: {error_count}")
    logging.info(
        f"Files skipped (e.g., already in destination, not found): {skipped_count}"
    )
    logging.info(f"Duplicate files deleted from source: {duplicate_deleted_count}")
    logging.info("------------------------")
    logging.info("Media sorting process finished.")


def main():
    # UnidentifiedImageError is now handled globally
    sort_photos()


if __name__ == "__main__":
    main()
