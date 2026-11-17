// flpdump: prints what Henosis reads from an FL Studio project file.
//
//   flpdump <project.flp> [--project-folder <dir>] [--events]
//
// --project-folder  classify samples as inside or outside this folder
//                   (defaults to the folder that contains the .flp)
// --events          also list every raw event: index, id, offset, size

#include <cstdio>
#include <filesystem>
#include <string>
#include <string_view>
#include <vector>

#include "fl/flp_reader.h"

#ifdef _WIN32
#ifndef NOMINMAX
#define NOMINMAX
#endif
#include <windows.h>
#endif

namespace {

const char* location_label(henosis::fl::SampleLocation location) {
    switch (location) {
        case henosis::fl::SampleLocation::Factory:
            return "factory";
        case henosis::fl::SampleLocation::InsideProject:
            return "inside ";
        case henosis::fl::SampleLocation::Outside:
            return "OUTSIDE";
    }
    return "?";
}

void print_usage() {
    std::fputs("usage: flpdump <project.flp> [--project-folder <dir>] [--events]\n", stderr);
}

}  // namespace

int main(int argc, char** argv) {
#ifdef _WIN32
    SetConsoleOutputCP(CP_UTF8);  // the reader returns UTF-8 strings
#endif

    std::string file;
    std::string project_folder;
    bool show_events = false;

    for (int i = 1; i < argc; ++i) {
        const std::string_view arg = argv[i];
        if (arg == "--events") {
            show_events = true;
        } else if (arg == "--project-folder") {
            if (i + 1 >= argc) {
                print_usage();
                return 2;
            }
            project_folder = argv[++i];
        } else if (file.empty()) {
            file = arg;
        } else {
            print_usage();
            return 2;
        }
    }
    if (file.empty()) {
        print_usage();
        return 2;
    }

    const std::filesystem::path path(file);
    if (project_folder.empty()) {
        project_folder = std::filesystem::absolute(path).parent_path().string();
    }

    std::vector<henosis::fl::FlpEvent> events;
    const henosis::fl::FlpResult result = henosis::fl::read_flp_file(path, &events);

    if (show_events) {
        std::printf("%6s  %3s  %8s  %8s\n", "index", "id", "offset", "size");
        for (std::size_t i = 0; i < events.size(); ++i) {
            std::printf("%6zu  %3u  %8zu  %8zu\n", i, static_cast<unsigned>(events[i].id),
                        events[i].offset, events[i].size);
        }
        std::printf("\n");
    }

    if (!result.ok) {
        std::fprintf(stderr, "flpdump: %s: unreadable: %s\n", file.c_str(), result.error.c_str());
        return 1;
    }

    const henosis::fl::FlpInfo& info = result.info;
    std::printf("File:        %s\n", file.c_str());
    std::printf("FL version:  %s\n", info.fl_version.c_str());
    std::printf("Title:       %s\n", info.title.empty() ? "(none)" : info.title.c_str());
    std::printf("Data path:   %s\n", info.data_path.empty() ? "(none)" : info.data_path.c_str());
    std::printf("Header:      format %d, %u channel(s), PPQ %u, %zu events\n",
                static_cast<int>(info.format), static_cast<unsigned>(info.channel_count),
                static_cast<unsigned>(info.ppq), info.event_count);

    std::printf("\nNative plugins (%zu)\n", info.native_plugins.size());
    for (const std::string& name : info.native_plugins) std::printf("  %s\n", name.c_str());

    std::printf("\nVST plugins (%zu)\n", info.vst_plugins.size());
    for (const henosis::fl::VstPlugin& plugin : info.vst_plugins) {
        std::printf("  %s", plugin.name.c_str());
        if (!plugin.vendor.empty()) std::printf("  [%s]", plugin.vendor.c_str());
        std::printf("\n");
        if (!plugin.path.empty()) std::printf("      %s\n", plugin.path.c_str());
    }
    if (info.unparsed_vst_count > 0) {
        std::printf("  (%zu wrapper plugin(s) could not be identified)\n", info.unparsed_vst_count);
    }

    std::size_t outside = 0;
    std::printf("\nSamples (%zu), relative to %s\n", info.sample_paths.size(), project_folder.c_str());
    for (const std::string& sample : info.sample_paths) {
        const auto location = henosis::fl::classify_sample(sample, project_folder);
        if (location == henosis::fl::SampleLocation::Outside) ++outside;
        std::printf("  [%s] %s\n", location_label(location), sample.c_str());
    }
    if (outside > 0) {
        std::printf("\n%zu sample(s) live outside the project folder. A partner will not have them.\n",
                    outside);
    }
    return 0;
}
