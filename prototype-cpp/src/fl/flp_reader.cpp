#include "fl/flp_reader.h"

#include <algorithm>
#include <cstring>
#include <fstream>
#include <optional>
#include <system_error>
#include <utility>

namespace henosis::fl {
namespace {

using Bytes = std::span<const std::uint8_t>;

// Event IDs, named as in PyFLP.
constexpr std::uint8_t kEventTitle = 194;         // ProjectID.Title
constexpr std::uint8_t kEventSamplePath = 196;    // ChannelID.SamplePath
constexpr std::uint8_t kEventVersion = 199;       // ProjectID.FLVersion
constexpr std::uint8_t kEventInternalName = 201;  // PluginID.InternalName
constexpr std::uint8_t kEventDataPath = 202;      // ProjectID.DataPath
constexpr std::uint8_t kEventPluginData = 213;    // PluginID.Data

// Sub-event IDs inside the data of a wrapper plugin.
constexpr std::uint32_t kVstSubName = 54;
constexpr std::uint32_t kVstSubPath = 55;
constexpr std::uint32_t kVstSubVendor = 56;

constexpr std::size_t kEventsStart = 22;  // 14-byte header chunk + "FLdt" + size
constexpr std::uintmax_t kMaxFileSize = 256ull * 1024ull * 1024ull;
constexpr std::string_view kVstWrapperName = "Fruity Wrapper";
constexpr std::string_view kFactoryPlaceholder = "%flstudiofactorydata%";

std::uint16_t read_u16(Bytes b, std::size_t at) {
    return static_cast<std::uint16_t>(b[at] | (b[at + 1] << 8));
}

std::uint32_t read_u32(Bytes b, std::size_t at) {
    return static_cast<std::uint32_t>(b[at]) |
           (static_cast<std::uint32_t>(b[at + 1]) << 8) |
           (static_cast<std::uint32_t>(b[at + 2]) << 16) |
           (static_cast<std::uint32_t>(b[at + 3]) << 24);
}

std::uint64_t read_u64(Bytes b, std::size_t at) {
    return static_cast<std::uint64_t>(read_u32(b, at)) |
           (static_cast<std::uint64_t>(read_u32(b, at + 4)) << 32);
}

void append_utf8(std::string& out, char32_t cp) {
    if (cp < 0x80) {
        out.push_back(static_cast<char>(cp));
    } else if (cp < 0x800) {
        out.push_back(static_cast<char>(0xC0 | (cp >> 6)));
        out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    } else if (cp < 0x10000) {
        out.push_back(static_cast<char>(0xE0 | (cp >> 12)));
        out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
        out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    } else {
        out.push_back(static_cast<char>(0xF0 | (cp >> 18)));
        out.push_back(static_cast<char>(0x80 | ((cp >> 12) & 0x3F)));
        out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
        out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
    }
}

// UTF-16LE to UTF-8. Trailing NUL terminators are dropped; a lone surrogate
// becomes U+FFFD. The caller has already checked that the size is even.
std::string decode_utf16le(Bytes v) {
    std::size_t units = v.size() / 2;
    while (units > 0 && read_u16(v, 2 * (units - 1)) == 0) --units;

    std::string out;
    out.reserve(units);
    for (std::size_t i = 0; i < units; ++i) {
        char32_t cp = read_u16(v, 2 * i);
        if (cp >= 0xD800 && cp <= 0xDBFF) {
            char32_t low = (i + 1 < units) ? read_u16(v, 2 * (i + 1)) : 0;
            if (low >= 0xDC00 && low <= 0xDFFF) {
                cp = 0x10000 + ((cp - 0xD800) << 10) + (low - 0xDC00);
                ++i;
            } else {
                cp = 0xFFFD;
            }
        } else if (cp >= 0xDC00 && cp <= 0xDFFF) {
            cp = 0xFFFD;
        }
        append_utf8(out, cp);
    }
    return out;
}

// 8-bit text (projects saved before FL Studio 11.5), read as Latin-1.
std::string decode_8bit(Bytes v) {
    std::size_t n = v.size();
    while (n > 0 && v[n - 1] == 0) --n;

    std::string out;
    out.reserve(n);
    for (std::size_t i = 0; i < n; ++i) append_utf8(out, v[i]);
    return out;
}

// Text stored as UTF-8 inside wrapper plugin data.
std::string copy_utf8(Bytes v) {
    std::size_t n = v.size();
    while (n > 0 && v[n - 1] == 0) --n;
    if (n == 0) return {};
    return std::string(reinterpret_cast<const char*>(v.data()), n);
}

// Text events are UTF-16LE from FL Studio 11.5 on and 8-bit before that.
// Returns nothing when the version string does not start with a number.
std::optional<bool> version_uses_utf16(std::string_view version) {
    std::size_t i = 0;
    auto read_number = [&]() -> std::optional<int> {
        int value = 0;
        bool any = false;
        while (i < version.size() && version[i] >= '0' && version[i] <= '9') {
            value = value * 10 + (version[i] - '0');
            if (value > 1'000'000) return std::nullopt;
            any = true;
            ++i;
        }
        if (!any) return std::nullopt;
        return value;
    };

    const auto major = read_number();
    if (!major) return std::nullopt;
    int minor = 0;
    if (i < version.size() && version[i] == '.') {
        ++i;
        minor = read_number().value_or(0);
    }
    return *major > 11 || (*major == 11 && minor >= 5);
}

// The data of a wrapper plugin: a 32-bit type, then sub-events made of a
// 32-bit id, a 64-bit length and the payload. Returns false when the bytes do
// not have exactly that shape or carry no plugin name.
bool parse_wrapper_data(Bytes v, VstPlugin& out) {
    if (v.size() < 4) return false;
    std::size_t pos = 4;
    while (pos < v.size()) {
        if (v.size() - pos < 12) return false;
        const std::uint32_t id = read_u32(v, pos);
        const std::uint64_t length = read_u64(v, pos + 4);
        pos += 12;
        if (length > v.size() - pos) return false;
        const Bytes payload = v.subspan(pos, static_cast<std::size_t>(length));
        pos += static_cast<std::size_t>(length);

        if (id == kVstSubName) {
            out.name = copy_utf8(payload);
        } else if (id == kVstSubPath) {
            out.path = copy_utf8(payload);
        } else if (id == kVstSubVendor) {
            out.vendor = copy_utf8(payload);
        }
    }
    return !out.name.empty();
}

template <typename T>
void push_unique(std::vector<T>& list, T value) {
    if (std::find(list.begin(), list.end(), value) == list.end()) {
        list.push_back(std::move(value));
    }
}

FlpResult fail(std::string reason) {
    FlpResult result;
    result.error = std::move(reason);
    return result;
}

// Lower-cases ASCII letters and turns backslashes into forward slashes.
std::string normalise_path(std::string_view path) {
    std::string out(path);
    for (char& c : out) {
        if (c == '\\') {
            c = '/';
        } else if (c >= 'A' && c <= 'Z') {
            c = static_cast<char>(c - 'A' + 'a');
        }
    }
    while (!out.empty() && out.back() == '/') out.pop_back();
    return out;
}

}  // namespace

FlpResult read_flp(Bytes bytes, std::vector<FlpEvent>* events_out) {
    if (events_out) events_out->clear();

    if (bytes.size() < kEventsStart) return fail("the file is too small to be a project");
    if (std::memcmp(bytes.data(), "FLhd", 4) != 0) return fail("missing FLhd header");
    if (read_u32(bytes, 4) != 6) return fail("unexpected header size");
    if (std::memcmp(bytes.data() + 14, "FLdt", 4) != 0) return fail("missing FLdt chunk");

    const std::uint32_t data_size = read_u32(bytes, 18);
    if (data_size > bytes.size() - kEventsStart) {
        return fail("the data chunk is longer than the file");
    }
    const std::size_t end = kEventsStart + data_size;

    // Pass 1: split the data chunk into events without interpreting them.
    std::vector<FlpEvent> events;
    std::size_t pos = kEventsStart;
    while (pos < end) {
        const std::uint8_t id = bytes[pos++];
        std::size_t size = 0;
        if (id < 64) {
            size = 1;
        } else if (id < 128) {
            size = 2;
        } else if (id < 192) {
            size = 4;
        } else {
            // Length prefix: 7 bits per byte, least significant group first,
            // top bit set on every byte but the last.
            std::uint64_t length = 0;
            int shift = 0;
            for (;;) {
                if (pos >= end) {
                    if (events_out) *events_out = std::move(events);
                    return fail("an event length is cut off");
                }
                const std::uint8_t byte = bytes[pos++];
                length |= static_cast<std::uint64_t>(byte & 0x7F) << shift;
                if ((byte & 0x80) == 0) break;
                shift += 7;
                if (shift > 28) {
                    if (events_out) *events_out = std::move(events);
                    return fail("an event length is too large");
                }
            }
            size = static_cast<std::size_t>(length);
        }
        if (size > end - pos) {
            if (events_out) *events_out = std::move(events);
            return fail("an event runs past the end of the data chunk");
        }
        events.push_back(FlpEvent{id, pos, size});
        pos += size;
    }

    FlpInfo info;
    info.format = static_cast<std::int16_t>(read_u16(bytes, 8));
    info.channel_count = read_u16(bytes, 10);
    info.ppq = read_u16(bytes, 12);
    info.event_count = events.size();

    auto value_of = [&](const FlpEvent& e) { return bytes.subspan(e.offset, e.size); };

    // The version string is always 8-bit and decides how other text is read.
    const auto version_event = std::find_if(events.begin(), events.end(), [](const FlpEvent& e) {
        return e.id == kEventVersion;
    });
    if (events_out) *events_out = events;
    if (version_event == events.end()) return fail("no FL Studio version in the file");
    info.fl_version = decode_8bit(value_of(*version_event));
    const auto utf16 = version_uses_utf16(info.fl_version);
    if (!utf16) return fail("the FL Studio version is not readable");

    bool bad_text = false;
    auto text_of = [&](const FlpEvent& e) -> std::string {
        const Bytes v = value_of(e);
        if (!*utf16) return decode_8bit(v);
        if (v.size() % 2 != 0) {
            bad_text = true;
            return {};
        }
        return decode_utf16le(v);
    };

    // Pass 2: pick out the few events Henosis uses.
    bool title_seen = false;
    bool data_path_seen = false;
    bool wrapper_pending = false;
    for (const FlpEvent& e : events) {
        switch (e.id) {
            case kEventTitle:
                if (!title_seen) {
                    info.title = text_of(e);
                    title_seen = true;
                }
                break;
            case kEventDataPath:
                if (!data_path_seen) {
                    info.data_path = text_of(e);
                    data_path_seen = true;
                }
                break;
            case kEventSamplePath: {
                std::string path = text_of(e);
                if (!path.empty()) push_unique(info.sample_paths, std::move(path));
                break;
            }
            case kEventInternalName: {
                std::string name = text_of(e);
                wrapper_pending = (name == kVstWrapperName);
                if (!wrapper_pending && !name.empty()) {
                    push_unique(info.native_plugins, std::move(name));
                }
                break;
            }
            case kEventPluginData:
                if (wrapper_pending) {
                    VstPlugin plugin;
                    if (parse_wrapper_data(value_of(e), plugin)) {
                        push_unique(info.vst_plugins, std::move(plugin));
                    } else {
                        ++info.unparsed_vst_count;
                    }
                    wrapper_pending = false;
                }
                break;
            default:
                break;
        }
        if (bad_text) return fail("a text event has an odd number of bytes");
    }

    FlpResult result;
    result.ok = true;
    result.info = std::move(info);
    return result;
}

FlpResult read_flp_file(const std::filesystem::path& path, std::vector<FlpEvent>* events_out) {
    if (events_out) events_out->clear();

    std::error_code ec;
    const std::uintmax_t size = std::filesystem::file_size(path, ec);
    if (ec) return fail("cannot read the file: " + ec.message());
    if (size > kMaxFileSize) return fail("the file is larger than 256 MiB");

    std::ifstream file(path, std::ios::binary);
    if (!file) return fail("cannot open the file");

    std::vector<std::uint8_t> bytes(static_cast<std::size_t>(size));
    if (size > 0) {
        file.read(reinterpret_cast<char*>(bytes.data()), static_cast<std::streamsize>(size));
        if (static_cast<std::uintmax_t>(file.gcount()) != size) {
            return fail("the file changed while it was being read");
        }
    }
    return read_flp(bytes, events_out);
}

SampleLocation classify_sample(std::string_view sample_path, std::string_view project_folder) {
    const std::string sample = normalise_path(sample_path);
    if (sample.find(kFactoryPlaceholder) != std::string::npos) return SampleLocation::Factory;

    const std::string folder = normalise_path(project_folder);
    if (!folder.empty() && sample.size() > folder.size() + 1 &&
        sample.compare(0, folder.size(), folder) == 0 && sample[folder.size()] == '/') {
        return SampleLocation::InsideProject;
    }
    return SampleLocation::Outside;
}

}  // namespace henosis::fl
