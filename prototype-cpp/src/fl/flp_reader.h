// Read-only parser for FL Studio project files (.flp).
//
// The format is not published by Image-Line. The layout used here follows the
// PyFLP documentation and was checked against a project saved by FL Studio
// 25.1.6. See "Desktop client > Reading .flp files" in the technical docs.
#pragma once

#include <cstddef>
#include <cstdint>
#include <filesystem>
#include <span>
#include <string>
#include <string_view>
#include <vector>

namespace henosis::fl {

// A plugin hosted through FL Studio's wrapper (VST2, VST3, ...).
struct VstPlugin {
    std::string name;
    std::string vendor;
    std::string path;  // where the plugin is installed on the saving machine

    bool operator==(const VstPlugin&) const = default;
};

// One raw event, kept for diagnostics (flpdump --events).
struct FlpEvent {
    std::uint8_t id = 0;
    std::size_t offset = 0;  // offset of the value within the file
    std::size_t size = 0;    // size of the value in bytes
};

// Everything Henosis needs from a project file. All strings are UTF-8.
struct FlpInfo {
    std::int16_t format = 0;
    std::uint16_t channel_count = 0;
    std::uint16_t ppq = 0;

    std::string fl_version;  // for example "25.1.6.4997"
    std::string title;       // from the project info; often empty
    std::string data_path;   // the project data folder, as stored

    std::vector<std::string> sample_paths;    // unique, in file order
    std::vector<std::string> native_plugins;  // unique internal names
    std::vector<VstPlugin> vst_plugins;       // unique

    // Wrapper plugins whose data did not have the expected layout. They are
    // counted, never guessed.
    std::size_t unparsed_vst_count = 0;

    std::size_t event_count = 0;
};

struct FlpResult {
    bool ok = false;
    FlpInfo info;       // meaningful only when ok is true
    std::string error;  // the reason when ok is false
};

// Parses a project held in memory. Never throws and never reads outside
// `bytes`. If the event stream is inconsistent in any way the result is an
// error, not a partial FlpInfo. When `events` is given it receives every raw
// event that was split out before the failure, or all of them on success.
FlpResult read_flp(std::span<const std::uint8_t> bytes,
                   std::vector<FlpEvent>* events = nullptr);

// Reads the file and parses it. Files above 256 MiB are refused.
FlpResult read_flp_file(const std::filesystem::path& path,
                        std::vector<FlpEvent>* events = nullptr);

enum class SampleLocation {
    Factory,        // ships with FL Studio; every install has it
    InsideProject,  // under the project folder, so it is synced
    Outside,        // somewhere else on disk; a partner will not have it
};

// Classifies a sample path taken from FlpInfo::sample_paths. The comparison
// ignores ASCII case and treats '/' and '\\' as the same separator.
SampleLocation classify_sample(std::string_view sample_path,
                               std::string_view project_folder);

}  // namespace henosis::fl
