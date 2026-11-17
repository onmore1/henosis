// henosis-cli: drives the sync engine against a folder that stands in for the
// server. This is the milestone 2 test tool, not the final app.
//
//   henosis-cli init    <folder> --remote <dir> [--user <name>]
//   henosis-cli status  <folder>
//   henosis-cli sync    <folder> [--yes]      pull, then push
//   henosis-cli push    <folder> [--yes]
//   henosis-cli pull    <folder>
//   henosis-cli log     <folder>
//   henosis-cli restore <folder> <snapshot-id>
//
// --yes allows a push that deletes more than half of the project's files.

#include <chrono>
#include <cstdio>
#include <filesystem>
#include <string>
#include <string_view>
#include <thread>
#include <vector>

#include "core/json.h"
#include "core/util.h"
#include "sync/folder_remote.h"
#include "sync/sync_engine.h"

#ifdef _WIN32
#ifndef NOMINMAX
#define NOMINMAX
#endif
#include <windows.h>
#endif

namespace fs = std::filesystem;
using namespace henosis;

namespace {

int usage() {
    std::fputs(
        "usage:\n"
        "  henosis-cli init    <folder> --remote <dir> [--user <name>]\n"
        "  henosis-cli status  <folder>\n"
        "  henosis-cli sync    <folder> [--yes]\n"
        "  henosis-cli push    <folder> [--yes]\n"
        "  henosis-cli pull    <folder>\n"
        "  henosis-cli log     <folder>\n"
        "  henosis-cli restore <folder> <snapshot-id>\n",
        stderr);
    return 2;
}

struct Config {
    std::string remote;
    std::string user;
};

bool load_config(const fs::path& folder, Config& config) {
    const auto text = core::read_text_file(folder / ".henosis" / "cli.json");
    if (!text) return false;
    const auto json = core::parse_json(*text);
    if (!json) return false;
    const auto remote = json->string_at("remote");
    const auto user = json->string_at("user");
    if (!remote || !user) return false;
    config.remote = *remote;
    config.user = *user;
    return true;
}

std::string short_id(const std::string& id) { return id.empty() ? "(none)" : id.substr(0, 8); }

int report(const char* what, const sync::SyncResult& result) {
    if (!result.ok) {
        std::fprintf(stderr, "%s failed: %s\n", what, result.error.c_str());
        if (result.kind == sync::ErrorKind::MassDelete) {
            std::fputs("Run again with --yes if that is what you want.\n", stderr);
        }
        return result.kind == sync::ErrorKind::Busy ? 3 : 1;
    }
    if (!result.pulled && !result.pushed) {
        std::printf("Up to date at version %s.\n", short_id(result.snapshot_id).c_str());
    } else {
        if (result.pulled) std::printf("Pulled: %zu file(s) downloaded.\n", result.downloaded);
        if (result.pushed) std::printf("Pushed: %zu file(s) uploaded.\n", result.uploaded);
        std::printf("Now at version %s.\n", short_id(result.snapshot_id).c_str());
    }
    for (const std::string& copy : result.conflict_copies) {
        std::printf("Conflict copy kept: %s\n", copy.c_str());
    }
    return 0;
}

// Runs an operation again while files are still being written.
template <typename Operation>
sync::SyncResult with_retry(Operation&& operation) {
    sync::SyncResult result;
    for (int attempt = 0; attempt < 10; ++attempt) {
        result = operation();
        if (result.ok || result.kind != sync::ErrorKind::Busy) break;
        std::this_thread::sleep_for(std::chrono::milliseconds(500));
    }
    return result;
}

void print_changes(const core::Diff& changes) {
    for (const auto& entry : changes.added) std::printf("  new       %s\n", entry.path.c_str());
    for (const auto& entry : changes.changed) std::printf("  changed   %s\n", entry.path.c_str());
    for (const auto& path : changes.removed) std::printf("  deleted   %s\n", path.c_str());
}

}  // namespace

int main(int argc, char** argv) {
#ifdef _WIN32
    SetConsoleOutputCP(CP_UTF8);
#endif
    if (argc < 3) return usage();

    const std::string_view command = argv[1];
    const fs::path folder = fs::absolute(fs::path(argv[2]));

    std::vector<std::string> rest;
    std::string remote_arg;
    std::string user_arg;
    bool yes = false;
    for (int i = 3; i < argc; ++i) {
        const std::string_view arg = argv[i];
        if (arg == "--yes") {
            yes = true;
        } else if (arg == "--remote" && i + 1 < argc) {
            remote_arg = argv[++i];
        } else if (arg == "--user" && i + 1 < argc) {
            user_arg = argv[++i];
        } else {
            rest.emplace_back(arg);
        }
    }

    if (command == "init") {
        if (remote_arg.empty()) return usage();
        std::error_code ec;
        if (!fs::is_directory(folder, ec)) {
            std::fprintf(stderr, "not a folder: %s\n", argv[2]);
            return 1;
        }
        const fs::path remote_path = fs::absolute(fs::path(remote_arg));
        sync::FolderRemote remote(remote_path);
        if (!remote.init()) {
            std::fprintf(stderr, "cannot create the remote folder: %s\n", remote_arg.c_str());
            return 1;
        }
        const std::string user = user_arg.empty() ? "me" : user_arg;
        const std::string json = "{\"remote\":" + core::json_string(core::path_to_utf8(remote_path)) +
                                 ",\"user\":" + core::json_string(user) + "}";
        if (!core::write_file_atomic(folder / ".henosis" / "cli.json", json)) {
            std::fputs("cannot write .henosis/cli.json\n", stderr);
            return 1;
        }
        std::printf("Linked %s to %s as %s.\n", argv[2], remote_arg.c_str(), user.c_str());
        return 0;
    }

    Config config;
    if (!load_config(folder, config)) {
        std::fprintf(stderr, "%s is not linked yet. Run: henosis-cli init <folder> --remote <dir>\n",
                     argv[2]);
        return 1;
    }
    sync::FolderRemote remote(core::path_from_utf8(config.remote));
    sync::SyncEngine engine(folder, remote, config.user);

    sync::PushOptions options;
    options.allow_mass_delete = yes;

    if (command == "sync") {
        return report("sync", with_retry([&] { return engine.sync(options); }));
    }
    if (command == "push") {
        return report("push", with_retry([&] { return engine.push(options); }));
    }
    if (command == "pull") {
        return report("pull", with_retry([&] { return engine.pull(); }));
    }
    if (command == "restore") {
        if (rest.size() != 1) return usage();
        // Accept the short id shown by "log".
        std::string id = rest[0];
        if (const auto history = remote.history(); history.ok) {
            for (const auto& snapshot : history.value) {
                if (snapshot.id.compare(0, id.size(), id) == 0) {
                    id = snapshot.id;
                    break;
                }
            }
        }
        return report("restore", with_retry([&] { return engine.restore(id); }));
    }
    if (command == "status") {
        const sync::Status status = engine.status();
        std::printf("Folder:  %s\n", core::path_to_utf8(folder).c_str());
        std::printf("Base:    %s\n", short_id(status.base_snapshot).c_str());
        if (!status.remote_reachable) {
            std::printf("Remote:  not reachable\n");
        } else {
            std::printf("Remote:  %s%s\n", short_id(status.head_snapshot).c_str(),
                        status.remote_ahead ? "  (newer than this folder)" : "");
        }
        switch (status.scan) {
            case sync::ScanStatus::Ok:
                if (status.local_changes.empty()) {
                    std::printf("Local:   no changes\n");
                } else {
                    std::printf("Local:   changes not uploaded\n");
                    print_changes(status.local_changes);
                }
                break;
            case sync::ScanStatus::FolderMissing:
                std::printf("Local:   the folder is missing\n");
                break;
            case sync::ScanStatus::SaveInProgress:
                std::printf("Local:   FL Studio is saving\n");
                break;
            case sync::ScanStatus::Unstable:
                std::printf("Local:   files are still changing\n");
                break;
            case sync::ScanStatus::Error:
                std::printf("Local:   cannot scan: %s\n", status.error.c_str());
                break;
        }
        if (status.conflict_pending) std::printf("Note:    conflict copies are waiting to be pushed\n");
        return 0;
    }
    if (command == "log") {
        const auto history = remote.history();
        if (!history.ok) {
            std::fprintf(stderr, "cannot read the history: %s\n", history.error.c_str());
            return 1;
        }
        if (history.value.empty()) std::printf("No versions yet.\n");
        for (const auto& snapshot : history.value) {
            std::printf("%s  %s  %-12s %s%s\n", short_id(snapshot.id).c_str(),
                        snapshot.created_at.c_str(), snapshot.author.c_str(),
                        snapshot.is_conflict ? "[conflict] " : "", snapshot.message.c_str());
        }
        return 0;
    }
    return usage();
}
