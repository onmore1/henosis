// The manifest: the complete file list of one version of a project.
// See "Sync protocol > Objects" in the technical docs.
#pragma once

#include <cstdint>
#include <string>
#include <string_view>
#include <vector>

namespace henosis::core {

struct FileEntry {
    std::string path;    // relative to the project folder, '/' separated, UTF-8
    std::string sha256;  // 64 lowercase hex characters
    std::uint64_t size = 0;

    bool operator==(const FileEntry&) const = default;
};

struct Manifest {
    static constexpr int kFormat = 1;

    // Kept sorted by path in byte order. Call sort() after adding entries.
    std::vector<FileEntry> files;

    void sort();
    const FileEntry* find(std::string_view path) const;  // needs sorted files

    // Canonical JSON: entries sorted by path, fixed key order, no whitespace.
    // Two clients that scan the same folder produce identical bytes.
    std::string to_json() const;

    // SHA-256 of to_json(): the name the manifest is stored under.
    std::string hash() const;

    bool operator==(const Manifest&) const = default;
};

// Returns an empty string when the manifest obeys every rule, otherwise the
// reason: a bad path, a bad hash, an ignored path, a duplicate, entries out of
// order, or two paths that differ only by case.
std::string validate_manifest(const Manifest& manifest);

struct ManifestParse {
    bool ok = false;
    Manifest manifest;
    std::string error;
};

// Parses manifest JSON, sorts the entries and validates the result. Unknown
// keys are ignored so fields can be added later.
ManifestParse parse_manifest(std::string_view json);

}  // namespace henosis::core
