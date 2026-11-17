// Rules for the relative paths that may appear in a manifest, and the list of
// paths that are never synced. See "Sync protocol > Paths, hashing and ignore
// rules" in the technical docs.
#pragma once

#include <string>
#include <string_view>

namespace henosis::core {

enum class PathError {
    None,
    Empty,
    InvalidUtf8,
    TooLong,             // more than 200 characters
    Absolute,            // leading '/'
    EmptySegment,        // "a//b" or a trailing '/'
    DotSegment,          // "." or ".."
    ControlCharacter,
    ForbiddenCharacter,  // < > : " | ? * or a backslash
    TrailingSpaceOrDot,
    ReservedName,        // CON, PRN, AUX, NUL, COM1-9, LPT1-9
};

// Checks one manifest path: relative, '/' separated, UTF-8.
PathError validate_path(std::string_view path);
const char* describe(PathError error);

// True for paths Henosis never uploads and never touches during a pull:
// its own metadata, FL Studio's backups, temp files and OS clutter.
bool is_ignored(std::string_view relative_path);

// True when the last segment is the temporary file FL Studio writes while it
// saves ("name.flp~"). A scan that sees one treats a save as in progress.
bool is_fl_save_in_progress(std::string_view relative_path);

// Lower-cases ASCII letters only. Used for case-insensitive comparisons.
std::string ascii_lower(std::string_view text);

bool is_valid_utf8(std::string_view text);
std::size_t count_code_points(std::string_view utf8);

}  // namespace henosis::core
