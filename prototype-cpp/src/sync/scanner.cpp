#include "sync/scanner.h"

#include <charconv>
#include <chrono>
#include <system_error>
#include <utility>

#include "core/json.h"
#include "core/path_rules.h"
#include "core/sha256.h"
#include "core/util.h"

namespace henosis::sync {
namespace fs = std::filesystem;

bool FileIndex::load(const fs::path& file) {
    entries.clear();
    const auto text = core::read_text_file(file);
    if (!text) return false;
    const auto root = core::parse_json(*text);
    if (!root) return false;
    const core::Json* files = root->get("files");
    if (!files || files->type != core::Json::Type::Array) return false;

    for (const core::Json& item : files->array) {
        const auto path = item.string_at("path");
        const auto size = item.integer_at("size");
        const auto mtime_text = item.string_at("mtime");
        const auto hash = item.string_at("sha256");
        if (!path || !size || !mtime_text || !hash) continue;
        std::int64_t mtime = 0;
        const auto [end, ec] =
            std::from_chars(mtime_text->data(), mtime_text->data() + mtime_text->size(), mtime);
        if (ec != std::errc() || end != mtime_text->data() + mtime_text->size()) continue;
        if (!core::is_sha256_hex(*hash)) continue;
        entries[*path] = IndexEntry{*size, mtime, *hash};
    }
    return true;
}

bool FileIndex::save(const fs::path& file) const {
    std::string out = "{\"files\":[";
    bool first = true;
    for (const auto& [path, entry] : entries) {
        if (!first) out.push_back(',');
        first = false;
        out += "{\"path\":" + core::json_string(path);
        out += ",\"size\":" + std::to_string(entry.size);
        // The tick count can be negative, so it is stored as text.
        out += ",\"mtime\":" + core::json_string(std::to_string(entry.mtime));
        out += ",\"sha256\":" + core::json_string(entry.sha256) + "}";
    }
    out += "]}";
    return core::write_file_atomic(file, out);
}

ScanResult scan_folder(const fs::path& root, FileIndex& index) {
    ScanResult result;
    std::error_code ec;
    if (!fs::is_directory(root, ec)) {
        result.status = ScanStatus::FolderMissing;
        return result;
    }

    auto fail = [&](ScanStatus status, std::string why = {}) {
        result.status = status;
        result.error = std::move(why);
        result.manifest.files.clear();
        return result;
    };

    using FileClock = fs::file_time_type::clock;
    const auto scan_started = FileClock::now();
    FileIndex fresh;

    fs::recursive_directory_iterator it(root, fs::directory_options::skip_permission_denied, ec);
    if (ec) return fail(ScanStatus::Error, "cannot list the folder: " + ec.message());

    for (const fs::recursive_directory_iterator end; it != end; it.increment(ec)) {
        if (ec) return fail(ScanStatus::Unstable);

        const fs::directory_entry& entry = *it;
        const std::string relative = core::path_to_utf8(entry.path().lexically_relative(root));

        std::error_code type_ec;
        if (entry.is_symlink(type_ec)) {
            if (it.recursion_pending()) it.disable_recursion_pending();
            continue;  // links are skipped, never followed
        }
        if (entry.is_directory(type_ec)) {
            if (core::is_ignored(relative)) it.disable_recursion_pending();
            continue;
        }
        if (!entry.is_regular_file(type_ec)) continue;

        if (core::is_fl_save_in_progress(relative)) return fail(ScanStatus::SaveInProgress);
        if (core::is_ignored(relative)) continue;
        if (core::validate_path(relative) != core::PathError::None) {
            result.skipped.push_back(relative);
            continue;
        }

        std::error_code stat_ec;
        const std::uintmax_t size = entry.file_size(stat_ec);
        if (stat_ec) return fail(ScanStatus::Unstable);
        const fs::file_time_type mtime = entry.last_write_time(stat_ec);
        if (stat_ec) return fail(ScanStatus::Unstable);
        const std::int64_t ticks = static_cast<std::int64_t>(mtime.time_since_epoch().count());

        std::string sha256;
        const auto cached = index.entries.find(relative);
        if (cached != index.entries.end() && cached->second.size == size &&
            cached->second.mtime == ticks) {
            sha256 = cached->second.sha256;
        } else {
            const auto hashed = core::sha256_file(entry.path());
            if (!hashed) return fail(ScanStatus::Unstable);
            // The file must not have moved under us while it was read.
            const std::uintmax_t size_after = fs::file_size(entry.path(), stat_ec);
            if (stat_ec) return fail(ScanStatus::Unstable);
            const fs::file_time_type mtime_after = fs::last_write_time(entry.path(), stat_ec);
            if (stat_ec || hashed->size != size || size_after != size || mtime_after != mtime) {
                return fail(ScanStatus::Unstable);
            }
            sha256 = hashed->sha256;
            ++result.hashed;
        }

        result.manifest.files.push_back(core::FileEntry{relative, sha256, size});
        if (scan_started - mtime >= std::chrono::seconds(2)) {
            fresh.entries[relative] = IndexEntry{size, ticks, sha256};
        }
    }

    result.manifest.sort();
    const std::string problem = core::validate_manifest(result.manifest);
    if (!problem.empty()) return fail(ScanStatus::Error, problem);

    index = std::move(fresh);
    result.status = ScanStatus::Ok;
    return result;
}

}  // namespace henosis::sync
