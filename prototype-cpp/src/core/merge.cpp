#include "core/merge.h"

#include <set>
#include <utility>

#include "core/path_rules.h"

namespace henosis::core {
namespace {

// Content at a path in one manifest: the hash, or empty when the path is absent.
std::string_view content_at(const Manifest& manifest, std::string_view path) {
    const FileEntry* entry = manifest.find(path);
    return entry ? std::string_view(entry->sha256) : std::string_view();
}

std::string safe_user(std::string_view user) {
    std::string out;
    for (const char c : user) {
        const bool keep = (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
                          (c >= '0' && c <= '9') || c == '-' || c == '_';
        out.push_back(keep ? c : '_');
    }
    return out.empty() ? std::string("unknown") : out;
}

}  // namespace

Diff diff(const Manifest& from, const Manifest& to) {
    Diff result;
    std::size_t i = 0;
    std::size_t j = 0;
    while (i < from.files.size() || j < to.files.size()) {
        if (j == to.files.size() ||
            (i < from.files.size() && from.files[i].path < to.files[j].path)) {
            result.removed.push_back(from.files[i].path);
            ++i;
        } else if (i == from.files.size() || to.files[j].path < from.files[i].path) {
            result.added.push_back(to.files[j]);
            ++j;
        } else {
            if (from.files[i].sha256 != to.files[j].sha256) result.changed.push_back(to.files[j]);
            ++i;
            ++j;
        }
    }
    return result;
}

MergePlan plan_merge(const Manifest& base, const Manifest& local, const Manifest& head) {
    std::set<std::string_view> paths;
    for (const Manifest* manifest : {&base, &local, &head}) {
        for (const FileEntry& entry : manifest->files) paths.insert(entry.path);
    }

    MergePlan plan;
    for (const std::string_view path : paths) {
        const std::string_view b = content_at(base, path);
        const std::string_view l = content_at(local, path);
        const std::string_view h = content_at(head, path);

        if (l == h) continue;  // both sides agree, whatever happened

        MergeStep step;
        step.path = std::string(path);
        step.local_exists = !l.empty();
        if (const FileEntry* entry = head.find(path)) step.head = *entry;

        if (l == b) {
            // Only the head changed.
            step.action = h.empty() ? MergeAction::DeleteLocal : MergeAction::TakeHead;
        } else if (h == b) {
            // Only the local side changed (edited, created or deleted).
            step.action = MergeAction::KeepLocal;
            plan.has_draft = true;
        } else if (l.empty()) {
            // Deleted locally, changed on the head: nothing local to preserve.
            step.action = MergeAction::TakeHead;
        } else {
            step.action = MergeAction::Conflict;
            ++plan.conflicts;
            plan.has_draft = true;  // the conflict copy still has to be pushed
        }
        plan.steps.push_back(std::move(step));
    }
    return plan;
}

std::string conflict_copy_name(std::string_view path, std::string_view user,
                               std::string_view date,
                               const std::function<bool(std::string_view)>& taken) {
    const std::size_t slash = path.rfind('/');
    const std::string_view folder =
        slash == std::string_view::npos ? std::string_view() : path.substr(0, slash + 1);
    const std::string_view leaf = slash == std::string_view::npos ? path : path.substr(slash + 1);

    // Split "take.wav" into "take" and ".wav"; a leading dot is not an extension.
    const std::size_t dot = leaf.rfind('.');
    const bool has_extension = dot != std::string_view::npos && dot != 0;
    const std::string_view stem = has_extension ? leaf.substr(0, dot) : leaf;
    const std::string_view extension = has_extension ? leaf.substr(dot) : std::string_view();

    const std::string label = "conflict, " + safe_user(user) + ", " + std::string(date);
    for (int counter = 1;; ++counter) {
        std::string name(folder);
        name += stem;
        name += " (";
        name += label;
        if (counter > 1) name += " " + std::to_string(counter);
        name += ")";
        name += extension;
        if (!taken(name)) return name;
    }
}

}  // namespace henosis::core
