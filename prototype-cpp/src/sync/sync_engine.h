// Push, pull and restore for one project folder.
// See "Sync protocol" in the technical docs.
//
// Local state lives in <folder>/.henosis/:
//   base.json    the manifest of the last synced version (the base)
//   state.json   the id of that version, and whether a conflict commit is due
//   index.json   the scanner's cache
//   tmp/         downloads in progress
//   trash/       files a pull removed
#pragma once

#include <cstddef>
#include <filesystem>
#include <functional>
#include <string>
#include <vector>

#include "core/manifest.h"
#include "core/merge.h"
#include "sync/remote.h"
#include "sync/scanner.h"

namespace henosis::sync {

enum class ErrorKind {
    None,
    FolderMissing,  // the project folder is gone; nothing was changed anywhere
    Busy,           // files are still being written; try again shortly
    ScanFailed,
    RemoteFailed,
    HeadMoved,      // someone committed first; pull, then push again
    UploadFailed,
    DownloadFailed,
    BlobCorrupt,    // downloaded bytes do not match their hash
    MassDelete,     // the push would delete more than half the files; needs a yes
    InvalidManifest,
    NotFound,
    LocalIo,
};

struct SyncResult {
    bool ok = false;
    ErrorKind kind = ErrorKind::None;
    std::string error;

    bool pushed = false;  // a new snapshot was committed
    bool pulled = false;  // the local folder was brought to a newer head
    std::size_t uploaded = 0;
    std::size_t downloaded = 0;
    std::vector<std::string> conflict_copies;  // paths created by this operation
    std::string snapshot_id;                   // the base after the operation
};

struct PushOptions {
    bool allow_mass_delete = false;
    std::string message;
};

struct Status {
    ScanStatus scan = ScanStatus::Error;
    std::string error;
    core::Diff local_changes;     // disk compared with the base
    std::string base_snapshot;
    std::string head_snapshot;    // empty when the remote has no snapshot
    bool remote_reachable = false;
    bool remote_ahead = false;    // the head is not the base
    bool conflict_pending = false;
};

class SyncEngine {
public:
    SyncEngine(std::filesystem::path folder, Remote& remote, std::string user);

    // Commits the folder as a new snapshot if it differs from the base.
    SyncResult push(const PushOptions& options = {});

    // Brings the folder to the head using the three-way rule. Local changes
    // survive as a draft or as conflict copies; nothing is discarded.
    SyncResult pull();

    // pull, then push, repeating when another commit lands in between.
    SyncResult sync(const PushOptions& options = {});

    // Commits the manifest of an older snapshot as a new version, then pulls.
    // History is never rewritten.
    SyncResult restore(const std::string& snapshot_id);

    Status status();

    const std::string& base_snapshot() const { return base_snapshot_; }
    const core::Manifest& base() const { return base_; }

    // The date used in conflict copy names. Tests replace it.
    void set_today(std::function<std::string()> today) { today_ = std::move(today); }

private:
    struct Scan {
        bool ok = false;
        core::Manifest manifest;
        SyncResult failure;
    };

    Scan scan();
    bool save_state();
    SyncResult upload(const std::vector<std::string>& hashes, const core::Manifest& local,
                      SyncResult result);

    std::filesystem::path folder_;
    std::filesystem::path meta_;
    Remote& remote_;
    std::string user_;
    std::function<std::string()> today_;

    core::Manifest base_;
    std::string base_snapshot_;
    bool conflict_pending_ = false;
    FileIndex index_;
};

}  // namespace henosis::sync
