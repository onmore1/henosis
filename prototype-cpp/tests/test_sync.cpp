// Integration tests for client/src/sync: two project folders and a folder
// remote on one machine, through edits, renames, deletes, conflicts and
// failures. Every test works in its own temporary directory.

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <filesystem>
#include <fstream>
#include <iterator>
#include <map>
#include <memory>
#include <random>
#include <set>
#include <string>
#include <string_view>
#include <vector>

#include "check.h"
#include "core/path_rules.h"
#include "core/sha256.h"
#include "core/util.h"
#include "sync/folder_remote.h"
#include "sync/scanner.h"
#include "sync/sync_engine.h"

namespace fs = std::filesystem;
using namespace henosis;
using sync::ErrorKind;
using sync::SyncResult;

namespace {

// --- Helpers ------------------------------------------------------------------

struct TempDir {
    fs::path path;
    TempDir() : path(fs::temp_directory_path() / ("henosis-test-" + core::random_uuid())) {
        fs::create_directories(path);
    }
    ~TempDir() {
        std::error_code ec;
        fs::remove_all(path, ec);
    }
    TempDir(const TempDir&) = delete;
    TempDir& operator=(const TempDir&) = delete;
};

void write_file(const fs::path& path, std::string_view content) {
    fs::create_directories(path.parent_path());
    std::ofstream file(path, std::ios::binary | std::ios::trunc);
    file.write(content.data(), static_cast<std::streamsize>(content.size()));
}

std::string read_file(const fs::path& path) {
    std::ifstream file(path, std::ios::binary);
    return std::string((std::istreambuf_iterator<char>(file)), std::istreambuf_iterator<char>());
}

// Relative path -> content for every synced file (ignored paths left out).
std::map<std::string, std::string> tree(const fs::path& root) {
    std::map<std::string, std::string> out;
    if (!fs::is_directory(root)) return out;
    for (const auto& entry : fs::recursive_directory_iterator(root)) {
        if (!entry.is_regular_file()) continue;
        const std::string relative = core::path_to_utf8(entry.path().lexically_relative(root));
        if (core::is_ignored(relative)) continue;
        out[relative] = read_file(entry.path());
    }
    return out;
}

// A remote that counts calls and can be told to fail, wrapped around a real one.
class FlakyRemote : public sync::Remote {
public:
    explicit FlakyRemote(sync::Remote& inner) : inner_(inner) {}

    int uploads = 0;
    int downloads = 0;
    int commits = 0;
    int fail_upload_at = 0;    // the n-th upload from now fails; 0 = never
    int fail_download_at = 0;
    bool lose_next_commit_response = false;

    sync::Outcome<std::optional<sync::SnapshotInfo>> head() override { return inner_.head(); }
    sync::Outcome<std::vector<std::string>> missing_blobs(const std::vector<std::string>& hashes) override {
        return inner_.missing_blobs(hashes);
    }
    std::string upload_blob(const std::string& sha256, const fs::path& file) override {
        if (fail_upload_at > 0 && --fail_upload_at == 0) return "simulated network failure";
        ++uploads;
        return inner_.upload_blob(sha256, file);
    }
    std::string download_blob(const std::string& sha256, const fs::path& destination) override {
        if (fail_download_at > 0 && --fail_download_at == 0) return "simulated network failure";
        ++downloads;
        return inner_.download_blob(sha256, destination);
    }
    sync::CommitResult commit(const sync::CommitRequest& request) override {
        ++commits;
        sync::CommitResult result = inner_.commit(request);
        if (lose_next_commit_response) {
            lose_next_commit_response = false;
            sync::CommitResult lost;
            lost.error = "simulated lost response";
            return lost;
        }
        return result;
    }
    sync::Outcome<core::Manifest> manifest(const std::string& id) override { return inner_.manifest(id); }
    sync::Outcome<std::vector<sync::SnapshotInfo>> history() override { return inner_.history(); }

private:
    sync::Remote& inner_;
};

// Two project folders ("ann" and "bob") sharing one remote.
struct World {
    TempDir dir;
    fs::path a = dir.path / "ann" / "My Song";
    fs::path b = dir.path / "bob" / "My Song";
    sync::FolderRemote storage{dir.path / "remote"};
    FlakyRemote remote{storage};
    std::unique_ptr<sync::SyncEngine> ann;
    std::unique_ptr<sync::SyncEngine> bob;

    World() {
        fs::create_directories(a);
        fs::create_directories(b);
        CHECK(storage.init());
        reopen_ann();
        reopen_bob();
    }
    // Recreating an engine reloads its state from disk, like an app restart.
    void reopen_ann() {
        ann = std::make_unique<sync::SyncEngine>(a, remote, "ann");
        ann->set_today([] { return std::string("2026-11-15"); });
    }
    void reopen_bob() {
        bob = std::make_unique<sync::SyncEngine>(b, remote, "bob");
        bob->set_today([] { return std::string("2026-11-15"); });
    }
    std::string head_id() {
        const auto head = storage.head();
        return head.ok && head.value ? head.value->id : std::string();
    }
    std::size_t history_size() { return storage.history().value.size(); }
};

const std::string kConflictCopy = "My Song (conflict, bob, 2026-11-15).flp";

// --- Scanner --------------------------------------------------------------------

void test_scanner() {
    TempDir dir;
    const fs::path root = dir.path / "proj";
    write_file(root / "My Song.flp", "project");
    write_file(root / "Audio" / "kick.wav", "kick");
    write_file(root / "Audio" / "empty.wav", "");
    write_file(root / "Backup" / "My Song (overwritten at 19h26).flp", "old");
    write_file(root / ".henosis" / "state.json", "{}");
    write_file(root / "Thumbs.db", "junk");
    write_file(root / core::path_from_utf8("Audio/Malibu \xE2\x80\x94 Camargue.mp3"), "music");

    sync::FileIndex index;
    sync::ScanResult scan = sync::scan_folder(root, index);
    CHECK(scan.status == sync::ScanStatus::Ok);
    CHECK(scan.manifest.files.size() == 4);
    CHECK(scan.hashed == 4);
    CHECK(scan.manifest.find("My Song.flp") != nullptr);
    CHECK(scan.manifest.find("Audio/kick.wav") && scan.manifest.find("Audio/kick.wav")->sha256 == core::sha256_hex("kick"));
    CHECK(scan.manifest.find("Audio/empty.wav") && scan.manifest.find("Audio/empty.wav")->size == 0);
    CHECK(scan.manifest.find("Audio/Malibu \xE2\x80\x94 Camargue.mp3") != nullptr);
    CHECK(scan.manifest.find("Thumbs.db") == nullptr);
    CHECK(core::validate_manifest(scan.manifest).empty());

    // Fresh files are not cached, so a rewrite within the same timestamp tick
    // can never be missed.
    CHECK(sync::scan_folder(root, index).hashed == 4);

    // Once a file is older than two seconds it is hashed only when it changes.
    const auto old = fs::file_time_type::clock::now() - std::chrono::seconds(30);
    for (const auto& entry : scan.manifest.files) fs::last_write_time(root / core::path_from_utf8(entry.path), old);
    CHECK(sync::scan_folder(root, index).hashed == 4);  // times changed: hashed once more
    CHECK(sync::scan_folder(root, index).hashed == 0);  // now served from the index

    write_file(root / "Audio" / "kick.wav", "KICK");  // same size, new time
    scan = sync::scan_folder(root, index);
    CHECK(scan.hashed == 1);
    CHECK(scan.manifest.find("Audio/kick.wav")->sha256 == core::sha256_hex("KICK"));

    // The index survives a save and load, negative tick counts included.
    index.entries["x"] = sync::IndexEntry{5, -123456789012345, std::string(64, 'a')};
    CHECK(index.save(dir.path / "index.json"));
    sync::FileIndex loaded;
    CHECK(loaded.load(dir.path / "index.json"));
    CHECK(loaded.entries.size() == index.entries.size());
    CHECK(loaded.entries["x"].mtime == -123456789012345 && loaded.entries["x"].size == 5);

    // FL Studio's temporary file means a save is under way.
    write_file(root / "My Song.flp~", "half written");
    CHECK(sync::scan_folder(root, index).status == sync::ScanStatus::SaveInProgress);
    fs::remove(root / "My Song.flp~");
    CHECK(sync::scan_folder(root, index).status == sync::ScanStatus::Ok);

    // A missing folder is reported as missing, never as an empty project.
    CHECK(sync::scan_folder(dir.path / "nope", index).status == sync::ScanStatus::FolderMissing);

    // Symbolic links are skipped. Creating one can be refused (Windows without
    // developer mode), in which case there is nothing to check.
    std::error_code ec;
    fs::create_symlink(root / "My Song.flp", root / "link.flp", ec);
    if (!ec) {
        scan = sync::scan_folder(root, index);
        CHECK(scan.status == sync::ScanStatus::Ok && scan.manifest.find("link.flp") == nullptr);
    }
}

// --- The folder remote enforces the server's rules ----------------------------------

void test_folder_remote_rules() {
    TempDir dir;
    sync::FolderRemote remote(dir.path / "remote");
    CHECK(remote.init());
    CHECK(remote.head().ok && !remote.head().value);

    const fs::path file = dir.path / "kick.wav";
    write_file(file, "kick");
    const std::string hash = core::sha256_hex("kick");

    // Bytes that do not match the claimed hash are refused and leave nothing behind.
    CHECK(!remote.upload_blob(std::string(64, 'f'), file).empty());
    CHECK(remote.missing_blobs({std::string(64, 'f')}).value.size() == 1);
    CHECK(!remote.upload_blob("not-a-hash", file).empty());

    core::Manifest manifest;
    manifest.files.push_back({"Audio/kick.wav", hash, 4});

    sync::CommitRequest request;
    request.manifest = manifest;
    request.author = "ann";
    CHECK(remote.commit(request).status == sync::CommitStatus::BlobsMissing);
    CHECK(remote.upload_blob(hash, file).empty());

    core::Manifest wrong_size = manifest;
    wrong_size.files[0].size = 5;
    request.manifest = wrong_size;
    CHECK(remote.commit(request).status == sync::CommitStatus::BlobsMissing);

    core::Manifest bad_path = manifest;
    bad_path.files[0].path = "../escape.wav";
    request.manifest = bad_path;
    CHECK(remote.commit(request).status == sync::CommitStatus::InvalidManifest);

    request.manifest = manifest;
    const sync::CommitResult first = remote.commit(request);
    CHECK(first.status == sync::CommitStatus::Committed);
    CHECK(remote.head().value && remote.head().value->id == first.snapshot.id);
    CHECK(remote.manifest(first.snapshot.id).ok && remote.manifest(first.snapshot.id).value == manifest);

    // The same request again is recognised as a retry, not a second version.
    CHECK(remote.commit(request).status == sync::CommitStatus::AlreadyCommitted);
    CHECK(remote.history().value.size() == 1);

    // A different commit on the old (empty) parent is refused.
    request.author = "bob";
    const sync::CommitResult stale = remote.commit(request);
    CHECK(stale.status == sync::CommitStatus::HeadMoved && stale.head_snapshot_id == first.snapshot.id);

    request.parent_id = first.snapshot.id;
    request.manifest = core::Manifest{};
    const sync::CommitResult second = remote.commit(request);
    CHECK(second.status == sync::CommitStatus::Committed);
    const auto history = remote.history();
    CHECK(history.value.size() == 2 && history.value[0].id == second.snapshot.id &&
          history.value[1].id == first.snapshot.id);

    CHECK(!remote.manifest("no-such-snapshot").ok);
    CHECK(!remote.manifest("../../etc/passwd").ok);
    CHECK(!remote.download_blob(std::string(64, 'e'), dir.path / "x").empty());
}

// --- Basic two-folder sync --------------------------------------------------------

void test_push_then_clone() {
    World w;
    write_file(w.a / "My Song.flp", "v1");
    write_file(w.a / "Audio" / "kick.wav", "kick");
    write_file(w.a / "Audio" / "same.wav", "kick");  // identical content: stored once

    SyncResult r = w.ann->push();
    CHECK(r.ok && r.pushed && r.uploaded == 2);
    CHECK(w.remote.uploads == 2);
    CHECK(w.history_size() == 1);

    CHECK(w.ann->push().ok && !w.ann->push().pushed);  // nothing changed: no new version
    CHECK(w.history_size() == 1);

    r = w.bob->pull();
    CHECK(r.ok && r.pulled && r.downloaded == 2);
    CHECK(tree(w.a) == tree(w.b));
    CHECK(w.bob->base_snapshot() == w.head_id());
    CHECK(w.bob->pull().ok && !w.bob->pull().pulled);
    CHECK(w.bob->status().local_changes.empty() && !w.bob->status().remote_ahead);
}

void test_edits_renames_deletes() {
    World w;
    write_file(w.a / "My Song.flp", "v1");
    write_file(w.a / "Audio" / "kick.wav", "kick");
    write_file(w.a / "Audio" / "snare.wav", "snare");
    write_file(w.a / "Audio" / "hat.wav", "hat");
    CHECK(w.ann->sync().ok);
    CHECK(w.bob->sync().ok);

    // Edit: only the changed file travels.
    write_file(w.b / "My Song.flp", "v2");
    w.remote.uploads = 0;
    CHECK(w.bob->sync().ok);
    CHECK(w.remote.uploads == 1);
    w.remote.downloads = 0;
    CHECK(w.ann->sync().ok);
    CHECK(w.remote.downloads == 1);
    CHECK(tree(w.a) == tree(w.b));

    // Rename and move: same content, so nothing is uploaded or downloaded.
    fs::create_directories(w.a / "Audio" / "Drums");
    fs::rename(w.a / "Audio" / "kick.wav", w.a / "Audio" / "Drums" / "kick 01.wav");
    w.remote.uploads = 0;
    w.remote.downloads = 0;
    CHECK(w.ann->sync().ok);
    CHECK(w.bob->sync().ok);
    CHECK(w.remote.uploads == 0 && w.remote.downloads == 0);
    CHECK(tree(w.a) == tree(w.b));
    CHECK(!fs::exists(w.b / "Audio" / "kick.wav"));
    CHECK(read_file(w.b / "Audio" / "Drums" / "kick 01.wav") == "kick");

    // Delete: the file leaves the partner's folder and lands in the trash.
    fs::remove(w.b / "Audio" / "snare.wav");
    fs::remove(w.b / "Audio" / "Drums" / "kick 01.wav");
    CHECK(w.bob->sync().ok);
    CHECK(w.ann->sync().ok);
    CHECK(tree(w.a) == tree(w.b));
    CHECK(!fs::exists(w.a / "Audio" / "snare.wav"));
    CHECK(!fs::exists(w.a / "Audio" / "Drums"));  // the emptied folder is removed
    bool in_trash = false;
    for (const auto& entry : fs::recursive_directory_iterator(w.a / ".henosis" / "trash")) {
        if (entry.is_regular_file() && entry.path().filename() == "snare.wav") {
            in_trash = read_file(entry.path()) == "snare";
        }
    }
    CHECK(in_trash);

    // Both sides agree with what the remote holds.
    const auto head_manifest = w.storage.manifest(w.head_id());
    CHECK(head_manifest.ok && head_manifest.value == w.ann->base() && head_manifest.value == w.bob->base());
}

void test_state_survives_restart() {
    World w;
    write_file(w.a / "My Song.flp", "v1");
    CHECK(w.ann->sync().ok);
    const std::string base = w.ann->base_snapshot();

    w.reopen_ann();
    CHECK(w.ann->base_snapshot() == base);
    CHECK(w.ann->status().local_changes.empty());
    CHECK(w.ann->sync().ok && w.history_size() == 1);

    write_file(w.a / "My Song.flp", "v2");
    w.reopen_ann();
    CHECK(w.ann->status().local_changes.changed.size() == 1);
    CHECK(w.ann->sync().ok && w.history_size() == 2);
}

// --- Conflicts ----------------------------------------------------------------------

void test_conflict_keeps_both_versions() {
    World w;
    write_file(w.a / "My Song.flp", "base");
    CHECK(w.ann->sync().ok);
    CHECK(w.bob->sync().ok);

    write_file(w.a / "My Song.flp", "ann's version");
    write_file(w.b / "My Song.flp", "bob's version");
    CHECK(w.ann->sync().ok);

    // Bob's plain push is refused; nothing on the remote changes.
    const std::string head_before = w.head_id();
    CHECK(w.bob->push().kind == ErrorKind::HeadMoved);
    CHECK(w.head_id() == head_before);

    const SyncResult r = w.bob->sync();
    CHECK(r.ok && r.pulled && r.pushed);
    CHECK(r.conflict_copies == std::vector<std::string>{kConflictCopy});
    CHECK(read_file(w.b / "My Song.flp") == "ann's version");
    CHECK(read_file(w.b / kConflictCopy) == "bob's version");

    // The commit that added the conflict copy is flagged in the history.
    const auto history = w.storage.history();
    CHECK(history.value.size() == 3 && history.value[0].is_conflict && !history.value[1].is_conflict);

    CHECK(w.ann->sync().ok);
    CHECK(tree(w.a) == tree(w.b));
    CHECK(read_file(w.a / kConflictCopy) == "bob's version");

    // A second conflict on the same day gets a counter.
    write_file(w.a / "My Song.flp", "ann again");
    write_file(w.b / "My Song.flp", "bob again");
    CHECK(w.ann->sync().ok);
    CHECK(w.bob->sync().ok);
    CHECK(read_file(w.b / "My Song (conflict, bob, 2026-11-15 2).flp") == "bob again");
    CHECK(read_file(w.b / kConflictCopy) == "bob's version");
    CHECK(read_file(w.b / "My Song.flp") == "ann again");
}

void test_merge_without_conflict() {
    World w;
    write_file(w.a / "My Song.flp", "base");
    write_file(w.a / "Audio" / "kick.wav", "kick");
    write_file(w.a / "Audio" / "old.wav", "old");
    CHECK(w.ann->sync().ok);
    CHECK(w.bob->sync().ok);

    // Different files on each side: both sets of changes survive, no copies.
    write_file(w.a / "My Song.flp", "ann edit");
    fs::remove(w.a / "Audio" / "old.wav");
    write_file(w.b / "Audio" / "kick.wav", "bob kick");
    write_file(w.b / "Audio" / "new.wav", "bob new");
    CHECK(w.ann->sync().ok);
    const SyncResult r = w.bob->sync();
    CHECK(r.ok && r.conflict_copies.empty());
    CHECK(w.ann->sync().ok);
    CHECK(tree(w.a) == tree(w.b));
    const auto t = tree(w.a);
    CHECK(t.size() == 3);
    CHECK(t.at("My Song.flp") == "ann edit" && t.at("Audio/kick.wav") == "bob kick" && t.at("Audio/new.wav") == "bob new");
    CHECK(!w.storage.history().value[0].is_conflict);

    // The same edit made on both sides is not a conflict.
    write_file(w.a / "My Song.flp", "same edit");
    write_file(w.b / "My Song.flp", "same edit");
    CHECK(w.ann->sync().ok);
    CHECK(w.bob->sync().ok && tree(w.a) == tree(w.b) && tree(w.b).size() == 3);
}

void test_delete_against_edit() {
    World w;
    write_file(w.a / "one.wav", "one");
    write_file(w.a / "two.wav", "two");
    write_file(w.a / "keep.wav", "keep");
    CHECK(w.ann->sync().ok);
    CHECK(w.bob->sync().ok);

    // Ann edits "one" and deletes "two"; Bob deletes "one" and edits "two".
    write_file(w.a / "one.wav", "one edited by ann");
    fs::remove(w.a / "two.wav");
    fs::remove(w.b / "one.wav");
    write_file(w.b / "two.wav", "two edited by bob");
    CHECK(w.ann->sync().ok);
    const SyncResult r = w.bob->sync();
    CHECK(r.ok);

    // Deleted here, edited there: the edit wins and the file comes back.
    CHECK(read_file(w.b / "one.wav") == "one edited by ann");
    // Edited here, deleted there: the edit is kept under a conflict name.
    CHECK(!fs::exists(w.b / "two.wav"));
    CHECK(read_file(w.b / "two (conflict, bob, 2026-11-15).wav") == "two edited by bob");
    CHECK(w.ann->sync().ok && tree(w.a) == tree(w.b));
}

void test_file_and_folder_swap() {
    World w;
    write_file(w.a / "x" / "inner.wav", "inner");
    write_file(w.a / "y", "plain file");
    for (const char* name : {"keep1.wav", "keep2.wav", "keep3.wav"}) write_file(w.a / name, name);
    write_file(w.a / core::path_from_utf8("Audio/Malibu \xE2\x80\x94 Camargue (official video).mp3"), "music");
    CHECK(w.ann->sync().ok);
    CHECK(w.bob->sync().ok);
    CHECK(tree(w.a) == tree(w.b));
    CHECK(tree(w.b).contains("Audio/Malibu \xE2\x80\x94 Camargue (official video).mp3"));

    // "x" turns from a folder into a file, "y" from a file into a folder.
    fs::remove_all(w.a / "x");
    write_file(w.a / "x", "now a file");
    fs::remove(w.a / "y");
    write_file(w.a / "y" / "deep" / "inner.wav", "now in a folder");
    CHECK(w.ann->sync().ok);
    const SyncResult r = w.bob->sync();
    CHECK(r.ok && r.conflict_copies.empty());
    CHECK(tree(w.a) == tree(w.b));
    CHECK(fs::is_regular_file(w.b / "x") && fs::is_directory(w.b / "y"));
}

// --- Failures ---------------------------------------------------------------------------

void test_interrupted_upload() {
    World w;
    write_file(w.a / "My Song.flp", "v1");
    CHECK(w.ann->sync().ok);
    const std::string head_before = w.head_id();

    write_file(w.a / "a.wav", "aaa");
    write_file(w.a / "b.wav", "bbb");
    write_file(w.a / "c.wav", "ccc");
    w.remote.fail_upload_at = 2;
    const SyncResult failed = w.ann->push();
    CHECK(!failed.ok && failed.kind == ErrorKind::UploadFailed);
    CHECK(w.head_id() == head_before);          // the head never saw a partial version
    CHECK(w.ann->base_snapshot() == head_before);

    w.remote.uploads = 0;
    const SyncResult retried = w.ann->push();
    CHECK(retried.ok && retried.pushed);
    CHECK(w.remote.uploads == 2);               // the blob that made it is not sent again
    CHECK(w.bob->sync().ok && tree(w.a) == tree(w.b));
}

void test_interrupted_download() {
    World w;
    write_file(w.a / "My Song.flp", "v1");
    write_file(w.a / "a.wav", "aaa");
    CHECK(w.ann->sync().ok);
    CHECK(w.bob->sync().ok);

    write_file(w.a / "My Song.flp", "v2");
    write_file(w.a / "a.wav", "AAA2");
    write_file(w.a / "b.wav", "bbb");
    CHECK(w.ann->sync().ok);

    const auto before = tree(w.b);
    const std::string base_before = w.bob->base_snapshot();
    w.remote.fail_download_at = 2;
    const SyncResult failed = w.bob->pull();
    CHECK(!failed.ok && failed.kind == ErrorKind::DownloadFailed);
    CHECK(tree(w.b) == before);                  // nothing was applied
    CHECK(w.bob->base_snapshot() == base_before);

    CHECK(w.bob->pull().ok);
    CHECK(tree(w.a) == tree(w.b));
}

void test_half_applied_pull_completes() {
    World w;
    write_file(w.a / "My Song.flp", "v1");
    write_file(w.a / "a.wav", "aaa");
    write_file(w.a / "gone.wav", "gone");
    CHECK(w.ann->sync().ok);
    CHECK(w.bob->sync().ok);

    write_file(w.a / "My Song.flp", "v2");
    write_file(w.a / "a.wav", "AAA2");
    fs::remove(w.a / "gone.wav");
    CHECK(w.ann->sync().ok);

    // As if the app had been killed after applying one file of the pull:
    // the file is already at the head's content, the base has not advanced.
    write_file(w.b / "a.wav", "AAA2");
    const SyncResult r = w.bob->pull();
    CHECK(r.ok && r.conflict_copies.empty());
    CHECK(tree(w.a) == tree(w.b));
    CHECK(w.bob->status().local_changes.empty());
}

void test_corrupt_blob_is_rejected() {
    World w;
    write_file(w.a / "My Song.flp", "v1");
    CHECK(w.ann->sync().ok);
    CHECK(w.bob->sync().ok);
    write_file(w.a / "My Song.flp", "v2 good bytes");
    CHECK(w.ann->sync().ok);

    // Damage the stored blob.
    write_file(w.storage.blob_path(core::sha256_hex("v2 good bytes")), "v2 BAD bytes!");
    const SyncResult r = w.bob->pull();
    CHECK(!r.ok && r.kind == ErrorKind::BlobCorrupt);
    CHECK(read_file(w.b / "My Song.flp") == "v1");  // the local file is untouched
}

void test_lost_commit_response() {
    World w;
    write_file(w.a / "My Song.flp", "v1");
    w.remote.lose_next_commit_response = true;
    const SyncResult lost = w.ann->push();
    CHECK(!lost.ok && lost.kind == ErrorKind::RemoteFailed);
    CHECK(w.history_size() == 1);  // the commit did land on the remote

    // The retry is answered with the existing snapshot instead of a second one.
    const SyncResult retried = w.ann->push();
    CHECK(retried.ok && retried.pushed);
    CHECK(w.history_size() == 1);
    CHECK(w.ann->base_snapshot() == w.head_id());
}

// --- Safety rules ---------------------------------------------------------------------------

void test_missing_folder_deletes_nothing() {
    World w;
    write_file(w.a / "My Song.flp", "v1");
    write_file(w.a / "a.wav", "aaa");
    CHECK(w.ann->sync().ok);
    const std::string head_before = w.head_id();

    fs::rename(w.a, w.dir.path / "moved away");
    CHECK(w.ann->push().kind == ErrorKind::FolderMissing);
    CHECK(w.ann->sync().kind == ErrorKind::FolderMissing);
    CHECK(w.ann->status().scan == sync::ScanStatus::FolderMissing);
    CHECK(w.head_id() == head_before && w.history_size() == 1);
}

void test_mass_delete_needs_a_yes() {
    World w;
    for (const char* name : {"a.wav", "b.wav", "c.wav", "d.wav"}) write_file(w.a / name, name);
    CHECK(w.ann->sync().ok);

    fs::remove(w.a / "a.wav");
    fs::remove(w.a / "b.wav");
    CHECK(w.ann->push().ok);  // exactly half: allowed

    fs::remove(w.a / "c.wav");
    fs::remove(w.a / "d.wav");
    const std::string head_before = w.head_id();
    const SyncResult refused = w.ann->push();
    CHECK(!refused.ok && refused.kind == ErrorKind::MassDelete);
    CHECK(w.head_id() == head_before);

    sync::PushOptions yes;
    yes.allow_mass_delete = true;
    CHECK(w.ann->push(yes).ok && w.head_id() != head_before);

    // Renaming or moving every file is not a deletion: the content is all there.
    for (const char* name : {"p.wav", "q.wav", "r.wav"}) write_file(w.a / "Audio" / name, name);
    CHECK(w.ann->push().ok);
    fs::rename(w.a / "Audio", w.a / "Samples");
    const SyncResult moved = w.ann->push();
    CHECK(moved.ok && moved.pushed && moved.uploaded == 0);
    CHECK(w.bob->sync().ok && tree(w.a) == tree(w.b) && tree(w.b).contains("Samples/q.wav"));
}

void test_ignored_files_stay_local() {
    World w;
    write_file(w.a / "My Song.flp", "v1");
    write_file(w.a / "Backup" / "My Song (overwritten at 19h26).flp", "older");
    write_file(w.a / "Thumbs.db", "junk");
    CHECK(w.ann->sync().ok);
    CHECK(w.bob->sync().ok);
    CHECK(tree(w.b).size() == 1);
    CHECK(!fs::exists(w.b / "Backup"));

    // Bob's own ignored files are left alone by a pull.
    write_file(w.b / "Backup" / "bob.flp", "bob's backup");
    write_file(w.a / "My Song.flp", "v2");
    CHECK(w.ann->sync().ok);
    CHECK(w.bob->sync().ok);
    CHECK(read_file(w.b / "Backup" / "bob.flp") == "bob's backup");

    // While FL Studio's temporary file exists, nothing is pushed.
    write_file(w.a / "My Song.flp~", "half");
    fs::remove(w.a / "My Song.flp");
    const std::string head_before = w.head_id();
    CHECK(w.ann->push().kind == ErrorKind::Busy);
    CHECK(w.head_id() == head_before);
    fs::rename(w.a / "My Song.flp~", w.a / "My Song.flp");
    CHECK(w.ann->push().ok);
    CHECK(w.bob->sync().ok && read_file(w.b / "My Song.flp") == "half");
}

void test_lost_base_is_safe() {
    World w;
    write_file(w.a / "My Song.flp", "v1");
    write_file(w.a / "a.wav", "aaa");
    CHECK(w.ann->sync().ok);
    CHECK(w.bob->sync().ok);
    write_file(w.b / "a.wav", "bob changed this");

    // Bob's local metadata disappears entirely.
    fs::remove_all(w.b / ".henosis");
    w.reopen_bob();
    CHECK(w.bob->base_snapshot().empty());
    const SyncResult r = w.bob->sync();
    CHECK(r.ok);
    // Identical files are simply adopted; the changed one is kept as a copy.
    CHECK(read_file(w.b / "My Song.flp") == "v1");
    CHECK(read_file(w.b / "a.wav") == "aaa");
    CHECK(read_file(w.b / "a (conflict, bob, 2026-11-15).wav") == "bob changed this");
}

// --- Restore ------------------------------------------------------------------------------------

void test_restore() {
    World w;
    write_file(w.a / "My Song.flp", "v1");
    write_file(w.a / "a.wav", "aaa");
    CHECK(w.ann->sync().ok);
    const std::string v1 = w.head_id();

    write_file(w.a / "My Song.flp", "v2");
    write_file(w.a / "b.wav", "bbb");
    fs::remove(w.a / "a.wav");
    CHECK(w.ann->sync().ok);
    CHECK(w.bob->sync().ok);

    // Uncommitted work at the time of the restore is committed first.
    write_file(w.a / "My Song.flp", "v3 unsaved to remote");
    const SyncResult r = w.ann->restore(v1);
    CHECK(r.ok);
    CHECK(w.history_size() == 4);  // v1, v2, v3, and the restore: history is not rewritten
    const auto t = tree(w.a);
    CHECK(t.size() == 2 && t.at("My Song.flp") == "v1" && t.at("a.wav") == "aaa");
    const auto history = w.storage.history();
    CHECK(history.value[0].message.rfind("Restored version from ", 0) == 0);
    CHECK(history.value[0].parent_id == history.value[1].id);
    CHECK(w.storage.manifest(history.value[1].id).value.find("My Song.flp")->sha256 ==
          core::sha256_hex("v3 unsaved to remote"));

    CHECK(w.bob->sync().ok && tree(w.a) == tree(w.b));
    CHECK(w.ann->restore("no-such-snapshot").kind == ErrorKind::NotFound);

    // Restoring the version that is already the head changes nothing.
    const std::size_t before = w.history_size();
    CHECK(w.ann->restore(w.head_id()).ok && w.history_size() == before);
}

// --- Random operations ------------------------------------------------------------------------------

std::set<std::string> content_hashes(const fs::path& root) {
    std::set<std::string> out;
    for (const auto& [path, content] : tree(root)) out.insert(core::sha256_hex(content));
    return out;
}

void test_random_operations(std::uint32_t runs) {
    const std::vector<std::string> names = {"My Song.flp",       "notes.txt",          "Audio/kick.wav",
                                            "Audio/snare.wav",   "Audio/vox take.wav", "Audio/Drums/hat.wav",
                                            "Renders/final.mp3", "Renders/demo.mp3"};
    sync::PushOptions yes;
    yes.allow_mass_delete = true;

    int syncs = 0;
    int conflicts = 0;
    for (std::uint32_t seed = 1; seed <= runs; ++seed) {
        std::mt19937 rng(seed);
        auto pick = [&](std::size_t n) { return static_cast<std::size_t>(rng() % n); };
        World w;
        int counter = 0;

        auto sync_side = [&](bool is_ann) {
            const fs::path& folder = is_ann ? w.a : w.b;
            const std::set<std::string> before = content_hashes(folder);
            if (pick(8) == 0) is_ann ? w.reopen_ann() : w.reopen_bob();  // app restart
            const SyncResult r = (is_ann ? *w.ann : *w.bob).sync(yes);
            CHECK(r.ok);
            ++syncs;
            conflicts += static_cast<int>(r.conflict_copies.size());
            // Nothing a side had on disk before a sync may be lost: every
            // content is either still there or stored on the remote.
            for (const std::string& hash : before) CHECK(fs::exists(w.storage.blob_path(hash)));
            // After a sync the folder is exactly the version it reports. With
            // no version yet (both sides still empty) the folder must be empty.
            if (r.snapshot_id.empty()) {
                CHECK(tree(folder).empty());
            } else {
                const auto head = w.storage.manifest(r.snapshot_id);
                std::set<std::string> expected;
                for (const auto& entry : head.value.files) expected.insert(entry.sha256);
                CHECK(head.ok);
                CHECK(content_hashes(folder) == expected);
                if (!head.ok || content_hashes(folder) != expected) {
                    std::printf("  seed %u: version %s, %zu file(s) on disk, %zu in the manifest\n",
                                seed, r.snapshot_id.c_str(), tree(folder).size(), head.value.files.size());
                }
            }
        };

        for (int step = 0; step < 60; ++step) {
            const bool is_ann = pick(2) == 0;
            const fs::path& folder = is_ann ? w.a : w.b;
            const auto current = tree(folder);
            const std::size_t op = pick(10);
            if (op < 5 || current.empty()) {
                // Write: mostly unique content, sometimes content both sides may produce.
                const std::string content = pick(4) == 0 ? "shared " + std::to_string(pick(3))
                                                         : "unique " + std::to_string(++counter);
                write_file(folder / core::path_from_utf8(names[pick(names.size())]), content);
            } else if (op < 7) {
                auto it = current.begin();
                std::advance(it, static_cast<std::ptrdiff_t>(pick(current.size())));
                fs::remove(folder / core::path_from_utf8(it->first));
            } else if (op < 9) {
                auto it = current.begin();
                std::advance(it, static_cast<std::ptrdiff_t>(pick(current.size())));
                const fs::path target = folder / core::path_from_utf8(names[pick(names.size())]);
                if (!fs::exists(target)) {
                    fs::create_directories(target.parent_path());
                    fs::rename(folder / core::path_from_utf8(it->first), target);
                }
            }
            if (pick(5) < 2) sync_side(is_ann);
        }

        // Settle: after each side has synced twice, both folders are identical.
        sync_side(true);
        sync_side(false);
        sync_side(true);
        sync_side(false);
        CHECK(tree(w.a) == tree(w.b));
        CHECK(w.ann->base_snapshot() == w.bob->base_snapshot());
        CHECK(w.ann->status().local_changes.empty() && w.bob->status().local_changes.empty());
    }
    std::printf("random operations: %u runs, %d syncs, %d conflict copies\n", runs, syncs, conflicts);
    CHECK(conflicts > 0);  // the runs really did exercise the conflict path
}

}  // namespace

// Usage: test_sync [random-runs]   (default 40)
int main(int argc, char** argv) {
    std::uint32_t runs = 40;
    if (argc > 1) runs = static_cast<std::uint32_t>(std::strtoul(argv[1], nullptr, 10));
    if (runs == 0) runs = 1;

    test_scanner();
    test_folder_remote_rules();
    test_push_then_clone();
    test_edits_renames_deletes();
    test_state_survives_restart();
    test_conflict_keeps_both_versions();
    test_merge_without_conflict();
    test_delete_against_edit();
    test_file_and_folder_swap();
    test_interrupted_upload();
    test_interrupted_download();
    test_half_applied_pull_completes();
    test_corrupt_blob_is_rejected();
    test_lost_commit_response();
    test_missing_folder_deletes_nothing();
    test_mass_delete_needs_a_yes();
    test_ignored_files_stay_local();
    test_lost_base_is_safe();
    test_restore();
    test_random_operations(runs);
    return henosis::test::finish("sync");
}
