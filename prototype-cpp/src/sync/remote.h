// What the sync engine needs from the place versions are stored. The calls
// mirror the HTTP API in the technical docs, so the real server client can
// replace the folder-based stand-in without touching the engine.
#pragma once

#include <filesystem>
#include <optional>
#include <string>
#include <vector>

#include "core/manifest.h"

namespace henosis::sync {

struct SnapshotInfo {
    std::string id;
    std::string parent_id;  // empty for the first snapshot
    std::string manifest_sha256;
    std::string author;
    std::string message;    // may be empty
    bool is_conflict = false;
    std::string created_at; // RFC 3339, UTC
};

template <typename T>
struct Outcome {
    bool ok = false;
    T value{};
    std::string error;
};

struct CommitRequest {
    std::string parent_id;  // the caller's base; empty when the project has no snapshot
    core::Manifest manifest;
    std::string author;
    std::string message;
    bool is_conflict = false;
};

enum class CommitStatus {
    Committed,
    AlreadyCommitted,  // a retry of a commit that had already succeeded
    HeadMoved,         // parent_id is not the head
    BlobsMissing,
    InvalidManifest,
    Failed,
};

struct CommitResult {
    CommitStatus status = CommitStatus::Failed;
    SnapshotInfo snapshot;             // for Committed and AlreadyCommitted
    std::string head_snapshot_id;      // for HeadMoved
    std::vector<std::string> missing;  // for BlobsMissing
    std::string error;                 // for InvalidManifest and Failed
};

class Remote {
public:
    virtual ~Remote() = default;

    // The latest snapshot, or nothing when the project is still empty.
    virtual Outcome<std::optional<SnapshotInfo>> head() = 0;

    // Which of these hashes are not stored yet.
    virtual Outcome<std::vector<std::string>> missing_blobs(
        const std::vector<std::string>& hashes) = 0;

    // Stores the file under `sha256`. Fails when the bytes do not match the
    // hash. Returns an error message, or an empty string on success.
    virtual std::string upload_blob(const std::string& sha256,
                                    const std::filesystem::path& file) = 0;

    // Writes the blob to `destination`. The caller verifies the content.
    virtual std::string download_blob(const std::string& sha256,
                                      const std::filesystem::path& destination) = 0;

    virtual CommitResult commit(const CommitRequest& request) = 0;

    virtual Outcome<core::Manifest> manifest(const std::string& snapshot_id) = 0;

    // Newest first.
    virtual Outcome<std::vector<SnapshotInfo>> history() = 0;
};

}  // namespace henosis::sync
