// Comparing manifests: a two-way diff and the three-way rule that decides what
// a pull does with each path. See "Sync protocol > Drafts and conflicts".
#pragma once

#include <cstddef>
#include <functional>
#include <optional>
#include <string>
#include <string_view>
#include <vector>

#include "core/manifest.h"

namespace henosis::core {

struct Diff {
    std::vector<FileEntry> added;     // in `to` only
    std::vector<FileEntry> changed;   // in both with different content; the `to` entry
    std::vector<std::string> removed; // in `from` only

    bool empty() const { return added.empty() && changed.empty() && removed.empty(); }
};

Diff diff(const Manifest& from, const Manifest& to);

enum class MergeAction {
    TakeHead,     // write the head's file at this path (create or replace)
    DeleteLocal,  // the head removed the file and the local copy is unchanged
    KeepLocal,    // only the local side changed: it stays as part of the draft
    Conflict,     // both sides changed: keep the local file under a conflict
                  // name, then give the path to the head's version, if any
};

struct MergeStep {
    std::string path;
    MergeAction action = MergeAction::KeepLocal;
    std::optional<FileEntry> head;  // the head's entry for this path, if it has one
    bool local_exists = false;
};

struct MergePlan {
    std::vector<MergeStep> steps;  // only paths that need attention, sorted by path
    std::size_t conflicts = 0;
    bool has_draft = false;        // true when local changes remain to be pushed
};

// base = the last synced version, local = what is on disk, head = the server.
// Paths where local and head already agree produce no step.
MergePlan plan_merge(const Manifest& base, const Manifest& local, const Manifest& head);

// "Audio/take.wav" -> "Audio/take (conflict, vadym, 2026-11-15).wav".
// `taken` says whether a candidate name is already in use; a counter is added
// until a free name is found. Characters in `user` that are not safe in a file
// name are replaced with '_'.
std::string conflict_copy_name(std::string_view path, std::string_view user,
                               std::string_view date,
                               const std::function<bool(std::string_view)>& taken);

}  // namespace henosis::core
