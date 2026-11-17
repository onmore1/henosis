#include "sync/sync_engine.h"

#include <map>
#include <set>
#include <system_error>
#include <utility>

#include "core/json.h"
#include "core/path_rules.h"
#include "core/sha256.h"
#include "core/util.h"

namespace henosis::sync {
namespace fs = std::filesystem;
namespace {

SyncResult failure(ErrorKind kind, std::string error) {
    SyncResult result;
    result.kind = kind;
    result.error = std::move(error);
    return result;
}

SyncResult failure(SyncResult result, ErrorKind kind, std::string error) {
    result.ok = false;
    result.kind = kind;
    result.error = std::move(error);
    return result;
}

// Removes `directory` and then its parents while they are empty, stopping at
// (and never removing) `stop`.
void remove_empty_parents(fs::path directory, const fs::path& stop) {
    std::error_code ec;
    while (directory != stop && directory.has_parent_path() && fs::is_empty(directory, ec) && !ec) {
        fs::remove(directory, ec);
        if (ec) return;
        directory = directory.parent_path();
    }
}

}  // namespace

SyncEngine::SyncEngine(fs::path folder, Remote& remote, std::string user)
    : folder_(std::move(folder)),
      meta_(folder_ / ".henosis"),
      remote_(remote),
      user_(std::move(user)),
      today_(core::today_date) {
    if (const auto text = core::read_text_file(meta_ / "state.json")) {
        if (const auto json = core::parse_json(*text)) {
            base_snapshot_ = json->string_at("base_snapshot").value_or("");
            conflict_pending_ = json->bool_at("conflict_pending").value_or(false);
        }
    }
    if (!base_snapshot_.empty()) {
        const auto text = core::read_text_file(meta_ / "base.json");
        auto parsed = text ? core::parse_manifest(*text) : core::ManifestParse{};
        if (parsed.ok) {
            base_ = std::move(parsed.manifest);
        } else {
            // Without its manifest the base is unknown. Starting from an empty
            // base is safe: every local file then counts as a local change.
            base_snapshot_.clear();
        }
    }
    index_.load(meta_ / "index.json");
}

bool SyncEngine::save_state() {
    std::string state = "{\"base_snapshot\":" + core::json_string(base_snapshot_);
    state += std::string(",\"conflict_pending\":") + (conflict_pending_ ? "true" : "false") + "}";
    // The manifest goes first: a state file must never name a base whose
    // manifest is not on disk.
    return core::write_file_atomic(meta_ / "base.json", base_.to_json()) &&
           core::write_file_atomic(meta_ / "state.json", state);
}

SyncEngine::Scan SyncEngine::scan() {
    Scan out;
    ScanResult scanned = scan_folder(folder_, index_);
    switch (scanned.status) {
        case ScanStatus::Ok:
            out.ok = true;
            out.manifest = std::move(scanned.manifest);
            index_.save(meta_ / "index.json");  // a cache; failing to save it is harmless
            break;
        case ScanStatus::FolderMissing:
            out.failure = failure(ErrorKind::FolderMissing,
                                  "the project folder is missing: " + core::path_to_utf8(folder_));
            break;
        case ScanStatus::SaveInProgress:
            out.failure = failure(ErrorKind::Busy, "FL Studio is saving the project");
            break;
        case ScanStatus::Unstable:
            out.failure = failure(ErrorKind::Busy, "files changed while they were being read");
            break;
        case ScanStatus::Error:
            out.failure = failure(ErrorKind::ScanFailed, scanned.error);
            break;
    }
    return out;
}

SyncResult SyncEngine::upload(const std::vector<std::string>& hashes, const core::Manifest& local,
                              SyncResult result) {
    std::map<std::string_view, std::string_view> path_by_hash;
    for (const core::FileEntry& entry : local.files) path_by_hash.emplace(entry.sha256, entry.path);

    for (const std::string& hash : hashes) {
        const auto found = path_by_hash.find(hash);
        if (found == path_by_hash.end()) {
            return failure(std::move(result), ErrorKind::UploadFailed,
                           "the remote asked for content this folder does not have: " + hash);
        }
        const std::string error =
            remote_.upload_blob(hash, folder_ / core::path_from_utf8(found->second));
        if (!error.empty()) {
            return failure(std::move(result), ErrorKind::UploadFailed, error);
        }
        ++result.uploaded;
    }
    result.ok = true;
    return result;
}

SyncResult SyncEngine::push(const PushOptions& options) {
    Scan scanned = scan();
    if (!scanned.ok) return scanned.failure;
    const core::Manifest& local = scanned.manifest;

    SyncResult result;
    result.snapshot_id = base_snapshot_;
    if (local == base_) {
        result.ok = true;
        return result;
    }

    // Guard against a folder that was emptied by accident. A file counts as
    // deleted only when its content is gone too, so renaming or moving files,
    // however many, never trips this.
    std::set<std::string_view> local_hashes;
    for (const core::FileEntry& entry : local.files) local_hashes.insert(entry.sha256);
    std::size_t deleted = 0;
    for (const core::FileEntry& entry : base_.files) {
        if (!local.find(entry.path) && !local_hashes.contains(entry.sha256)) ++deleted;
    }
    if (!options.allow_mass_delete && deleted * 2 > base_.files.size()) {
        return failure(ErrorKind::MassDelete,
                       "this push would delete " + std::to_string(deleted) + " of " +
                           std::to_string(base_.files.size()) + " files");
    }

    // Only content the base does not already have can be new to the remote.
    std::set<std::string_view> base_hashes;
    for (const core::FileEntry& entry : base_.files) base_hashes.insert(entry.sha256);
    std::set<std::string> candidates;
    for (const core::FileEntry& entry : local.files) {
        if (!base_hashes.contains(entry.sha256)) candidates.insert(entry.sha256);
    }
    const auto missing =
        remote_.missing_blobs(std::vector<std::string>(candidates.begin(), candidates.end()));
    if (!missing.ok) return failure(ErrorKind::RemoteFailed, missing.error);

    result = upload(missing.value, local, std::move(result));
    if (!result.ok) return result;
    result.ok = false;

    CommitRequest request;
    request.parent_id = base_snapshot_;
    request.manifest = local;
    request.author = user_;
    request.message = options.message;
    request.is_conflict = conflict_pending_;

    for (int attempt = 0; attempt < 2; ++attempt) {
        CommitResult commit = remote_.commit(request);
        switch (commit.status) {
            case CommitStatus::Committed:
            case CommitStatus::AlreadyCommitted:
                base_ = local;
                base_snapshot_ = commit.snapshot.id;
                conflict_pending_ = false;
                result.snapshot_id = base_snapshot_;
                result.pushed = true;
                if (!save_state()) {
                    return failure(std::move(result), ErrorKind::LocalIo,
                                   "the version was committed but the local state could not be saved");
                }
                result.ok = true;
                return result;
            case CommitStatus::HeadMoved:
                return failure(std::move(result), ErrorKind::HeadMoved,
                               "someone committed a newer version first");
            case CommitStatus::BlobsMissing:
                if (attempt == 0) {
                    result = upload(commit.missing, local, std::move(result));
                    if (!result.ok) return result;
                    result.ok = false;
                    continue;
                }
                return failure(std::move(result), ErrorKind::UploadFailed,
                               "the remote still reports missing content after a second upload");
            case CommitStatus::InvalidManifest:
                return failure(std::move(result), ErrorKind::InvalidManifest, commit.error);
            case CommitStatus::Failed:
                return failure(std::move(result), ErrorKind::RemoteFailed, commit.error);
        }
    }
    return failure(std::move(result), ErrorKind::RemoteFailed, "the commit did not complete");
}

SyncResult SyncEngine::pull() {
    const auto head = remote_.head();
    if (!head.ok) return failure(ErrorKind::RemoteFailed, head.error);

    SyncResult result;
    result.snapshot_id = base_snapshot_;
    if (!head.value || head.value->id == base_snapshot_) {
        result.ok = true;
        return result;
    }

    const auto head_manifest = remote_.manifest(head.value->id);
    if (!head_manifest.ok) return failure(ErrorKind::RemoteFailed, head_manifest.error);

    Scan scanned = scan();
    if (!scanned.ok) return scanned.failure;
    const core::Manifest& local = scanned.manifest;

    const core::MergePlan plan = core::plan_merge(base_, local, head_manifest.value);

    // --- 1. Stage every piece of content the plan needs, before touching the
    // project. A source file may itself be renamed or replaced further down.
    std::error_code ec;
    const fs::path tmp = meta_ / "tmp";
    fs::remove_all(tmp, ec);
    fs::create_directories(tmp, ec);
    if (ec) return failure(ErrorKind::LocalIo, "cannot create " + core::path_to_utf8(tmp));

    std::map<std::string, std::size_t> uses;       // hash -> number of paths that need it
    std::map<std::string, std::uint64_t> sizes;
    for (const core::MergeStep& step : plan.steps) {
        const bool needs_head = step.head && (step.action == core::MergeAction::TakeHead ||
                                              step.action == core::MergeAction::Conflict);
        if (needs_head) {
            ++uses[step.head->sha256];
            sizes[step.head->sha256] = step.head->size;
        }
    }
    std::map<std::string_view, std::string_view> local_by_hash;
    for (const core::FileEntry& entry : local.files) local_by_hash.emplace(entry.sha256, entry.path);

    for (const auto& [hash, count] : uses) {
        const fs::path staged = tmp / (hash + ".part");
        const auto local_copy = local_by_hash.find(hash);
        const bool from_local = local_copy != local_by_hash.end();
        if (from_local) {
            fs::copy_file(folder_ / core::path_from_utf8(local_copy->second), staged,
                          fs::copy_options::overwrite_existing, ec);
            if (ec) return failure(ErrorKind::Busy, "a file changed while it was being copied");
        } else {
            const std::string error = remote_.download_blob(hash, staged);
            if (!error.empty()) return failure(std::move(result), ErrorKind::DownloadFailed, error);
            ++result.downloaded;
        }
        const auto hashed = core::sha256_file(staged);
        if (!hashed || hashed->sha256 != hash || hashed->size != sizes[hash]) {
            if (from_local) return failure(ErrorKind::Busy, "a file changed while it was being copied");
            return failure(std::move(result), ErrorKind::BlobCorrupt,
                           "downloaded content does not match its hash: " + hash);
        }
    }

    // --- 2. Apply. Order: conflict renames, deletions, then new content, so a
    // path can change from a folder to a file and back.
    std::set<std::string> taken;  // lower-cased names that must not be reused
    for (const core::FileEntry& entry : local.files) taken.insert(core::ascii_lower(entry.path));
    for (const core::FileEntry& entry : head_manifest.value.files) {
        taken.insert(core::ascii_lower(entry.path));
    }
    auto is_taken = [&](std::string_view name) {
        std::error_code exists_ec;
        return taken.contains(core::ascii_lower(name)) ||
               fs::exists(folder_ / core::path_from_utf8(name), exists_ec);
    };

    const std::string date = today_();
    for (const core::MergeStep& step : plan.steps) {
        if (step.action != core::MergeAction::Conflict) continue;
        const std::string copy = core::conflict_copy_name(step.path, user_, date, is_taken);
        fs::rename(folder_ / core::path_from_utf8(step.path), folder_ / core::path_from_utf8(copy), ec);
        if (ec) {
            return failure(std::move(result), ErrorKind::LocalIo,
                           "cannot rename " + step.path + ": " + ec.message());
        }
        taken.insert(core::ascii_lower(copy));
        result.conflict_copies.push_back(copy);
    }

    fs::path trash;
    for (const core::MergeStep& step : plan.steps) {
        if (step.action != core::MergeAction::DeleteLocal) continue;
        if (trash.empty()) {
            trash = meta_ / "trash" / (core::now_rfc3339().substr(0, 10) + "-" + core::random_uuid().substr(0, 8));
        }
        const fs::path source = folder_ / core::path_from_utf8(step.path);
        const fs::path target = trash / core::path_from_utf8(step.path);
        fs::create_directories(target.parent_path(), ec);
        fs::rename(source, target, ec);
        if (ec) {
            return failure(std::move(result), ErrorKind::LocalIo,
                           "cannot move " + step.path + " to the trash: " + ec.message());
        }
        remove_empty_parents(source.parent_path(), folder_);
    }

    std::size_t apply_counter = 0;
    for (const core::MergeStep& step : plan.steps) {
        const bool places_head = step.head && (step.action == core::MergeAction::TakeHead ||
                                               step.action == core::MergeAction::Conflict);
        if (!places_head) continue;

        const fs::path target = folder_ / core::path_from_utf8(step.path);
        const fs::path staged = tmp / (step.head->sha256 + ".part");
        fs::create_directories(target.parent_path(), ec);

        fs::path source = staged;
        if (--uses[step.head->sha256] > 0) {
            // More paths need this content: place a copy and keep the staged file.
            source = tmp / ("apply-" + std::to_string(apply_counter++) + ".part");
            fs::copy_file(staged, source, fs::copy_options::overwrite_existing, ec);
            if (ec) {
                return failure(std::move(result), ErrorKind::LocalIo,
                               "cannot write " + step.path + ": " + ec.message());
            }
        }
        fs::rename(source, target, ec);  // replaces an existing file in one step
        if (ec) {
            return failure(std::move(result), ErrorKind::LocalIo,
                           "cannot write " + step.path + ": " + ec.message());
        }
    }
    fs::remove_all(tmp, ec);

    // --- 3. Only now does the base advance. If anything above failed, the
    // next pull starts from the same base and repeats the remaining steps.
    base_ = head_manifest.value;
    base_snapshot_ = head.value->id;
    if (!result.conflict_copies.empty()) conflict_pending_ = true;
    result.snapshot_id = base_snapshot_;
    result.pulled = true;
    if (!save_state()) {
        return failure(std::move(result), ErrorKind::LocalIo, "cannot save the local state");
    }
    result.ok = true;
    return result;
}

SyncResult SyncEngine::sync(const PushOptions& options) {
    SyncResult total;
    for (int attempt = 0; attempt < 5; ++attempt) {
        SyncResult pulled = pull();
        total.downloaded += pulled.downloaded;
        total.pulled = total.pulled || pulled.pulled;
        total.conflict_copies.insert(total.conflict_copies.end(), pulled.conflict_copies.begin(),
                                     pulled.conflict_copies.end());
        if (!pulled.ok) {
            pulled.downloaded = total.downloaded;
            pulled.uploaded = total.uploaded;
            pulled.conflict_copies = total.conflict_copies;
            pulled.pulled = total.pulled;
            return pulled;
        }

        SyncResult pushed = push(options);
        total.uploaded += pushed.uploaded;
        if (pushed.ok) {
            total.ok = true;
            total.pushed = pushed.pushed;
            total.snapshot_id = pushed.snapshot_id;
            return total;
        }
        if (pushed.kind != ErrorKind::HeadMoved) {
            pushed.downloaded = total.downloaded;
            pushed.uploaded = total.uploaded;
            pushed.conflict_copies = total.conflict_copies;
            pushed.pulled = total.pulled;
            return pushed;
        }
        // Someone committed between our pull and our push: go round again.
    }
    return failure(std::move(total), ErrorKind::HeadMoved,
                   "other commits kept arriving; try again");
}

SyncResult SyncEngine::restore(const std::string& snapshot_id) {
    // Local changes are committed first, so they stay in the history.
    SyncResult synced = sync();
    if (!synced.ok) return synced;

    const auto wanted = remote_.manifest(snapshot_id);
    if (!wanted.ok) return failure(ErrorKind::NotFound, wanted.error);
    if (wanted.value == base_) {
        synced.pushed = false;
        return synced;  // the head already has exactly this content
    }

    std::string when;
    if (const auto history = remote_.history(); history.ok) {
        for (const SnapshotInfo& snapshot : history.value) {
            if (snapshot.id == snapshot_id) when = snapshot.created_at;
        }
    }

    CommitRequest request;
    request.parent_id = base_snapshot_;
    request.manifest = wanted.value;
    request.author = user_;
    request.message = when.empty() ? "Restored an earlier version" : "Restored version from " + when;

    const CommitResult commit = remote_.commit(request);
    switch (commit.status) {
        case CommitStatus::Committed:
        case CommitStatus::AlreadyCommitted:
            break;
        case CommitStatus::HeadMoved:
            return failure(ErrorKind::HeadMoved, "someone committed a newer version first");
        case CommitStatus::BlobsMissing:
            return failure(ErrorKind::NotFound, "that version's content has expired on the remote");
        case CommitStatus::InvalidManifest:
            return failure(ErrorKind::InvalidManifest, commit.error);
        case CommitStatus::Failed:
            return failure(ErrorKind::RemoteFailed, commit.error);
    }

    // The folder still equals the old head, so this pull is a plain update.
    SyncResult pulled = pull();
    pulled.pushed = pulled.ok;
    pulled.uploaded += synced.uploaded;
    pulled.downloaded += synced.downloaded;
    return pulled;
}

Status SyncEngine::status() {
    Status out;
    out.base_snapshot = base_snapshot_;
    out.conflict_pending = conflict_pending_;

    ScanResult scanned = scan_folder(folder_, index_);
    out.scan = scanned.status;
    out.error = scanned.error;
    if (scanned.status == ScanStatus::Ok) {
        index_.save(meta_ / "index.json");
        out.local_changes = core::diff(base_, scanned.manifest);
    }

    const auto head = remote_.head();
    out.remote_reachable = head.ok;
    if (head.ok && head.value) {
        out.head_snapshot = head.value->id;
        out.remote_ahead = head.value->id != base_snapshot_;
    }
    return out;
}

}  // namespace henosis::sync
