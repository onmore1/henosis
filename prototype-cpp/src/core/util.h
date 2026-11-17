// Small helpers shared by the sync code: time, ids and file I/O.
#pragma once

#include <filesystem>
#include <optional>
#include <string>
#include <string_view>

namespace henosis::core {

// "2026-11-15T17:20:09Z", in UTC.
std::string now_rfc3339();
// "2026-11-15", in UTC.
std::string today_date();
// A random version 4 UUID in lowercase.
std::string random_uuid();

// Reads a whole file. Returns nothing when it cannot be read.
std::optional<std::string> read_text_file(const std::filesystem::path& path);

// Writes `content` to a temporary file beside `path` and renames it into
// place, so a crash never leaves a half-written file. Returns false on error.
bool write_file_atomic(const std::filesystem::path& path, std::string_view content);

// Conversions between UTF-8 strings and filesystem paths, correct on Windows
// where the native encoding is UTF-16.
std::filesystem::path path_from_utf8(std::string_view utf8);
std::string path_to_utf8(const std::filesystem::path& path);  // '/' separators

}  // namespace henosis::core
