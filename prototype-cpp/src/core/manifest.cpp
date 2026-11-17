#include "core/manifest.h"

#include <algorithm>
#include <set>

#include "core/json.h"
#include "core/path_rules.h"
#include "core/sha256.h"

namespace henosis::core {

void Manifest::sort() {
    std::sort(files.begin(), files.end(),
              [](const FileEntry& a, const FileEntry& b) { return a.path < b.path; });
}

const FileEntry* Manifest::find(std::string_view path) const {
    const auto it = std::lower_bound(
        files.begin(), files.end(), path,
        [](const FileEntry& entry, std::string_view wanted) { return entry.path < wanted; });
    if (it == files.end() || it->path != path) return nullptr;
    return &*it;
}

std::string Manifest::to_json() const {
    std::string out = "{\"format\":" + std::to_string(kFormat) + ",\"files\":[";
    bool first = true;
    for (const FileEntry& entry : files) {
        if (!first) out.push_back(',');
        first = false;
        out += "{\"path\":";
        out += json_string(entry.path);
        out += ",\"sha256\":";
        out += json_string(entry.sha256);
        out += ",\"size\":";
        out += std::to_string(entry.size);
        out.push_back('}');
    }
    out += "]}";
    return out;
}

std::string Manifest::hash() const { return sha256_hex(to_json()); }

std::string validate_manifest(const Manifest& manifest) {
    std::set<std::string> folded;
    const FileEntry* previous = nullptr;
    for (const FileEntry& entry : manifest.files) {
        const PathError path_error = validate_path(entry.path);
        if (path_error != PathError::None) {
            return std::string(describe(path_error)) + ": " + entry.path;
        }
        if (is_ignored(entry.path)) return "the path is on the ignore list: " + entry.path;
        if (!is_sha256_hex(entry.sha256)) return "bad hash for " + entry.path;
        if (previous && !(previous->path < entry.path)) {
            return previous->path == entry.path ? "duplicate path: " + entry.path
                                                : "entries are not sorted by path";
        }
        if (!folded.insert(ascii_lower(entry.path)).second) {
            return "two paths differ only by case: " + entry.path;
        }
        previous = &entry;
    }
    return {};
}

ManifestParse parse_manifest(std::string_view json) {
    ManifestParse result;
    const auto root = parse_json(json);
    if (!root || root->type != Json::Type::Object) {
        result.error = "the manifest is not a JSON object";
        return result;
    }
    const auto format = root->integer_at("format");
    if (!format || *format != static_cast<std::uint64_t>(Manifest::kFormat)) {
        result.error = "unsupported manifest format";
        return result;
    }
    const Json* files = root->get("files");
    if (!files || files->type != Json::Type::Array) {
        result.error = "the manifest has no files array";
        return result;
    }
    result.manifest.files.reserve(files->array.size());
    for (const Json& item : files->array) {
        const auto path = item.string_at("path");
        const auto hash = item.string_at("sha256");
        const auto size = item.integer_at("size");
        if (!path || !hash || !size) {
            result.error = "a file entry is missing path, sha256 or size";
            return result;
        }
        result.manifest.files.push_back(FileEntry{*path, *hash, *size});
    }
    result.manifest.sort();
    result.error = validate_manifest(result.manifest);
    result.ok = result.error.empty();
    return result;
}

}  // namespace henosis::core
