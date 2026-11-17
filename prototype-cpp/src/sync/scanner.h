// Turns a project folder on disk into a manifest.
// See "Sync protocol > Change detection" in the technical docs.
#pragma once

#include <cstdint>
#include <filesystem>
#include <map>
#include <string>
#include <vector>

#include "core/manifest.h"

namespace henosis::sync {

// What the scanner remembers about a file so it does not hash it again.
struct IndexEntry {
    std::uint64_t size = 0;
    std::int64_t mtime = 0;  // file clock ticks, only compared for equality
    std::string sha256;
};

// A cache, never a source of truth: deleting it only costs a full re-hash.
struct FileIndex {
    std::map<std::string, IndexEntry> entries;  // key: relative path

    bool load(const std::filesystem::path& file);
    bool save(const std::filesystem::path& file) const;
};

enum class ScanStatus {
    Ok,
    FolderMissing,   // the project folder is gone; never read this as "all deleted"
    SaveInProgress,  // FL Studio's temporary "name.flp~" is present; try again
    Unstable,        // a file changed or vanished while it was being read; try again
    Error,
};

struct ScanResult {
    ScanStatus status = ScanStatus::Error;
    core::Manifest manifest;            // valid when status is Ok
    std::string error;                  // set when status is Error
    std::size_t hashed = 0;             // files whose content was read this time
    std::vector<std::string> skipped;   // files left out because their path breaks a rule
};

// Walks `root`, skips ignored paths and links, and hashes only files whose
// size or modified time differ from `index`. On Ok, `index` is replaced with
// the fresh state. Files modified in the last two seconds are not cached, so a
// second write within the same timestamp tick can never be missed.
ScanResult scan_folder(const std::filesystem::path& root, FileIndex& index);

}  // namespace henosis::sync
