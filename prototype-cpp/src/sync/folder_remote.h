// A plain folder that stands in for the server. Used for milestone 2 and for
// tests; it applies the same rules the server will.
//
//   <root>/blobs/<sha256>          file contents and manifests
//   <root>/snapshots/<id>.json     one record per version
//   <root>/HEAD                    id of the latest snapshot
#pragma once

#include <filesystem>

#include "sync/remote.h"

namespace henosis::sync {

class FolderRemote : public Remote {
public:
    explicit FolderRemote(std::filesystem::path root);

    // Creates the folder layout. Returns false when it cannot be created.
    bool init();

    Outcome<std::optional<SnapshotInfo>> head() override;
    Outcome<std::vector<std::string>> missing_blobs(const std::vector<std::string>& hashes) override;
    std::string upload_blob(const std::string& sha256, const std::filesystem::path& file) override;
    std::string download_blob(const std::string& sha256,
                              const std::filesystem::path& destination) override;
    CommitResult commit(const CommitRequest& request) override;
    Outcome<core::Manifest> manifest(const std::string& snapshot_id) override;
    Outcome<std::vector<SnapshotInfo>> history() override;

    const std::filesystem::path& root() const { return root_; }
    std::filesystem::path blob_path(const std::string& sha256) const;

private:
    Outcome<std::string> read_head_id() const;
    Outcome<SnapshotInfo> read_snapshot(const std::string& id) const;

    std::filesystem::path root_;
};

}  // namespace henosis::sync
