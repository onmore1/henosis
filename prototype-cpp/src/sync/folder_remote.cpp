#include "sync/folder_remote.h"

#include <system_error>
#include <utility>

#include "core/json.h"
#include "core/sha256.h"
#include "core/util.h"

namespace henosis::sync {
namespace fs = std::filesystem;
namespace {

// Snapshot ids come from callers, so they are checked before they are used in
// a file name.
bool is_safe_id(const std::string& id) {
    if (id.empty() || id.size() > 64) return false;
    for (const char c : id) {
        const bool ok = (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-';
        if (!ok) return false;
    }
    return true;
}

std::string snapshot_json(const SnapshotInfo& s) {
    std::string out = "{\"id\":" + core::json_string(s.id);
    out += ",\"parent_id\":" + (s.parent_id.empty() ? "null" : core::json_string(s.parent_id));
    out += ",\"manifest_sha256\":" + core::json_string(s.manifest_sha256);
    out += ",\"author\":" + core::json_string(s.author);
    out += ",\"message\":" + (s.message.empty() ? "null" : core::json_string(s.message));
    out += std::string(",\"is_conflict\":") + (s.is_conflict ? "true" : "false");
    out += ",\"created_at\":" + core::json_string(s.created_at) + "}";
    return out;
}

}  // namespace

FolderRemote::FolderRemote(fs::path root) : root_(std::move(root)) {}

bool FolderRemote::init() {
    std::error_code ec;
    fs::create_directories(root_ / "blobs", ec);
    if (ec) return false;
    fs::create_directories(root_ / "snapshots", ec);
    return !ec;
}

fs::path FolderRemote::blob_path(const std::string& sha256) const {
    return root_ / "blobs" / sha256;
}

Outcome<std::string> FolderRemote::read_head_id() const {
    Outcome<std::string> out;
    std::error_code ec;
    const fs::path head_file = root_ / "HEAD";
    if (!fs::exists(head_file, ec)) {
        out.ok = !ec;
        if (ec) out.error = "cannot reach the remote: " + ec.message();
        return out;  // ok with an empty id: no snapshot yet
    }
    const auto text = core::read_text_file(head_file);
    if (!text) {
        out.error = "cannot read HEAD";
        return out;
    }
    std::string id = *text;
    while (!id.empty() && (id.back() == '\n' || id.back() == '\r' || id.back() == ' ')) id.pop_back();
    if (!is_safe_id(id)) {
        out.error = "HEAD does not hold a snapshot id";
        return out;
    }
    out.ok = true;
    out.value = std::move(id);
    return out;
}

Outcome<SnapshotInfo> FolderRemote::read_snapshot(const std::string& id) const {
    Outcome<SnapshotInfo> out;
    if (!is_safe_id(id)) {
        out.error = "not a snapshot id";
        return out;
    }
    const auto text = core::read_text_file(root_ / "snapshots" / (id + ".json"));
    if (!text) {
        out.error = "no such snapshot: " + id;
        return out;
    }
    const auto json = core::parse_json(*text);
    const auto manifest_hash = json ? json->string_at("manifest_sha256") : std::nullopt;
    if (!json || !manifest_hash || !core::is_sha256_hex(*manifest_hash)) {
        out.error = "the snapshot record is damaged: " + id;
        return out;
    }
    out.value.id = id;
    out.value.parent_id = json->string_at("parent_id").value_or("");
    out.value.manifest_sha256 = *manifest_hash;
    out.value.author = json->string_at("author").value_or("");
    out.value.message = json->string_at("message").value_or("");
    out.value.is_conflict = json->bool_at("is_conflict").value_or(false);
    out.value.created_at = json->string_at("created_at").value_or("");
    out.ok = true;
    return out;
}

Outcome<std::optional<SnapshotInfo>> FolderRemote::head() {
    Outcome<std::optional<SnapshotInfo>> out;
    const auto id = read_head_id();
    if (!id.ok) {
        out.error = id.error;
        return out;
    }
    if (id.value.empty()) {
        out.ok = true;
        return out;
    }
    const auto snapshot = read_snapshot(id.value);
    if (!snapshot.ok) {
        out.error = snapshot.error;
        return out;
    }
    out.ok = true;
    out.value = snapshot.value;
    return out;
}

Outcome<std::vector<std::string>> FolderRemote::missing_blobs(
    const std::vector<std::string>& hashes) {
    Outcome<std::vector<std::string>> out;
    std::error_code ec;
    for (const std::string& hash : hashes) {
        if (!core::is_sha256_hex(hash)) {
            out.error = "not a SHA-256 hash: " + hash;
            return out;
        }
        if (!fs::exists(blob_path(hash), ec)) out.value.push_back(hash);
    }
    out.ok = true;
    return out;
}

std::string FolderRemote::upload_blob(const std::string& sha256, const fs::path& file) {
    if (!core::is_sha256_hex(sha256)) return "not a SHA-256 hash: " + sha256;

    std::error_code ec;
    const fs::path final_path = blob_path(sha256);
    if (fs::exists(final_path, ec)) return {};  // already stored

    // Copy to a temporary name first, then check the bytes, as a storage
    // service does with a checksum header.
    const fs::path temp = root_ / "blobs" / (".upload-" + core::random_uuid());
    fs::copy_file(file, temp, fs::copy_options::overwrite_existing, ec);
    if (ec) return "cannot store the blob: " + ec.message();

    const auto hashed = core::sha256_file(temp);
    if (!hashed || hashed->sha256 != sha256) {
        fs::remove(temp, ec);
        return "the uploaded bytes do not match the hash " + sha256;
    }
    fs::rename(temp, final_path, ec);
    if (ec) {
        fs::remove(temp, ec);
        return "cannot store the blob: " + ec.message();
    }
    return {};
}

std::string FolderRemote::download_blob(const std::string& sha256, const fs::path& destination) {
    if (!core::is_sha256_hex(sha256)) return "not a SHA-256 hash: " + sha256;
    std::error_code ec;
    if (!fs::exists(blob_path(sha256), ec)) return "blob not found: " + sha256;
    fs::copy_file(blob_path(sha256), destination, fs::copy_options::overwrite_existing, ec);
    if (ec) return "cannot download the blob: " + ec.message();
    return {};
}

CommitResult FolderRemote::commit(const CommitRequest& request) {
    CommitResult result;

    core::Manifest manifest = request.manifest;
    manifest.sort();
    const std::string problem = core::validate_manifest(manifest);
    if (!problem.empty()) {
        result.status = CommitStatus::InvalidManifest;
        result.error = problem;
        return result;
    }
    const std::string manifest_json = manifest.to_json();
    const std::string manifest_hash = core::sha256_hex(manifest_json);

    const auto head_id = read_head_id();
    if (!head_id.ok) {
        result.error = head_id.error;
        return result;
    }
    if (head_id.value != request.parent_id) {
        // A retry whose first attempt succeeded: same parent, same manifest,
        // same author. Answer with the snapshot that already exists.
        if (!head_id.value.empty()) {
            const auto head = read_snapshot(head_id.value);
            if (head.ok && head.value.parent_id == request.parent_id &&
                head.value.manifest_sha256 == manifest_hash &&
                head.value.author == request.author) {
                result.status = CommitStatus::AlreadyCommitted;
                result.snapshot = head.value;
                return result;
            }
        }
        result.status = CommitStatus::HeadMoved;
        result.head_snapshot_id = head_id.value;
        return result;
    }

    // Every file must be in storage with the size the manifest declares.
    std::error_code ec;
    for (const core::FileEntry& entry : manifest.files) {
        const fs::path blob = blob_path(entry.sha256);
        const std::uintmax_t size = fs::file_size(blob, ec);
        if (ec || size != entry.size) result.missing.push_back(entry.sha256);
    }
    if (!result.missing.empty()) {
        result.status = CommitStatus::BlobsMissing;
        return result;
    }

    if (!fs::exists(blob_path(manifest_hash), ec) &&
        !core::write_file_atomic(blob_path(manifest_hash), manifest_json)) {
        result.error = "cannot store the manifest";
        return result;
    }

    SnapshotInfo snapshot;
    snapshot.id = core::random_uuid();
    snapshot.parent_id = request.parent_id;
    snapshot.manifest_sha256 = manifest_hash;
    snapshot.author = request.author;
    snapshot.message = request.message;
    snapshot.is_conflict = request.is_conflict;
    snapshot.created_at = core::now_rfc3339();

    // The record is written first; the head moves last, in one rename.
    if (!core::write_file_atomic(root_ / "snapshots" / (snapshot.id + ".json"),
                                 snapshot_json(snapshot)) ||
        !core::write_file_atomic(root_ / "HEAD", snapshot.id + "\n")) {
        result.error = "cannot write the snapshot";
        return result;
    }
    result.status = CommitStatus::Committed;
    result.snapshot = std::move(snapshot);
    return result;
}

Outcome<core::Manifest> FolderRemote::manifest(const std::string& snapshot_id) {
    Outcome<core::Manifest> out;
    const auto snapshot = read_snapshot(snapshot_id);
    if (!snapshot.ok) {
        out.error = snapshot.error;
        return out;
    }
    const auto text = core::read_text_file(blob_path(snapshot.value.manifest_sha256));
    if (!text) {
        out.error = "the manifest of snapshot " + snapshot_id + " is missing";
        return out;
    }
    if (core::sha256_hex(*text) != snapshot.value.manifest_sha256) {
        out.error = "the manifest of snapshot " + snapshot_id + " is damaged";
        return out;
    }
    auto parsed = core::parse_manifest(*text);
    if (!parsed.ok) {
        out.error = "the manifest of snapshot " + snapshot_id + " is invalid: " + parsed.error;
        return out;
    }
    out.ok = true;
    out.value = std::move(parsed.manifest);
    return out;
}

Outcome<std::vector<SnapshotInfo>> FolderRemote::history() {
    Outcome<std::vector<SnapshotInfo>> out;
    const auto head_id = read_head_id();
    if (!head_id.ok) {
        out.error = head_id.error;
        return out;
    }
    std::string id = head_id.value;
    // The chain ends at the first snapshot, or where older versions expired.
    while (!id.empty() && out.value.size() < 100000) {
        const auto snapshot = read_snapshot(id);
        if (!snapshot.ok) break;
        out.value.push_back(snapshot.value);
        id = snapshot.value.parent_id;
    }
    out.ok = true;
    return out;
}

}  // namespace henosis::sync
