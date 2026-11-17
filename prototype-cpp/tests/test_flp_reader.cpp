// Unit tests for the FLP reader. No test framework: each CHECK prints on
// failure and the process exits non-zero if any failed.

#include <cstdint>
#include <cstdio>
#include <string>
#include <string_view>
#include <vector>

#include "fl/flp_reader.h"

using henosis::fl::classify_sample;
using henosis::fl::FlpEvent;
using henosis::fl::FlpResult;
using henosis::fl::read_flp;
using henosis::fl::SampleLocation;
using henosis::fl::VstPlugin;

namespace {

int failures = 0;

#define CHECK(condition)                                                        \
    do {                                                                        \
        if (!(condition)) {                                                     \
            ++failures;                                                         \
            std::printf("FAILED %s:%d  %s\n", __FILE__, __LINE__, #condition);  \
        }                                                                       \
    } while (false)

using Buffer = std::vector<std::uint8_t>;

void put_u16(Buffer& b, std::uint16_t v) {
    b.push_back(static_cast<std::uint8_t>(v & 0xFF));
    b.push_back(static_cast<std::uint8_t>(v >> 8));
}

void put_u32(Buffer& b, std::uint32_t v) {
    for (int i = 0; i < 4; ++i) b.push_back(static_cast<std::uint8_t>((v >> (8 * i)) & 0xFF));
}

void put_u64(Buffer& b, std::uint64_t v) {
    for (int i = 0; i < 8; ++i) b.push_back(static_cast<std::uint8_t>((v >> (8 * i)) & 0xFF));
}

void put_bytes(Buffer& b, std::string_view s) { b.insert(b.end(), s.begin(), s.end()); }

// UTF-16LE with a NUL terminator. Input must be ASCII except for `extra`.
Buffer utf16(std::string_view ascii, std::vector<std::uint16_t> extra = {}) {
    Buffer out;
    for (char c : ascii) put_u16(out, static_cast<std::uint8_t>(c));
    for (std::uint16_t unit : extra) put_u16(out, unit);
    put_u16(out, 0);
    return out;
}

Buffer ascii_z(std::string_view s) {
    Buffer out(s.begin(), s.end());
    out.push_back(0);
    return out;
}

void event_byte(Buffer& b, std::uint8_t id, std::uint8_t v) {
    b.push_back(id);
    b.push_back(v);
}

void event_word(Buffer& b, std::uint8_t id, std::uint16_t v) {
    b.push_back(id);
    put_u16(b, v);
}

void event_dword(Buffer& b, std::uint8_t id, std::uint32_t v) {
    b.push_back(id);
    put_u32(b, v);
}

void event_var(Buffer& b, std::uint8_t id, const Buffer& value) {
    b.push_back(id);
    std::size_t n = value.size();
    do {
        std::uint8_t byte = static_cast<std::uint8_t>(n & 0x7F);
        n >>= 7;
        if (n != 0) byte |= 0x80;
        b.push_back(byte);
    } while (n != 0);
    b.insert(b.end(), value.begin(), value.end());
}

Buffer wrapper_data(std::string_view name, std::string_view path, std::string_view vendor) {
    Buffer d;
    put_u32(d, 12);  // type
    auto sub = [&](std::uint32_t id, std::string_view payload) {
        put_u32(d, id);
        put_u64(d, payload.size());
        put_bytes(d, payload);
    };
    sub(1, std::string(20, '\x01'));
    sub(54, name);
    sub(55, path);
    sub(56, vendor);
    sub(53, std::string(300, '\x02'));  // plugin state
    return d;
}

Buffer make_file(const Buffer& events) {
    Buffer f;
    put_bytes(f, "FLhd");
    put_u32(f, 6);
    put_u16(f, 0);   // format
    put_u16(f, 3);   // channels
    put_u16(f, 96);  // ppq
    put_bytes(f, "FLdt");
    put_u32(f, static_cast<std::uint32_t>(events.size()));
    f.insert(f.end(), events.begin(), events.end());
    return f;
}

Buffer modern_events() {
    Buffer e;
    event_var(e, 199, ascii_z("25.1.6.4997"));
    event_byte(e, 28, 1);
    event_word(e, 64, 0);
    event_dword(e, 128, 0x12345678);
    event_var(e, 194, utf16("My Song"));
    event_var(e, 202, utf16("C:\\Projects\\My Song\\"));

    // Channel 0: a sampler with a sample inside the project.
    event_var(e, 201, utf16(""));
    event_var(e, 196, utf16("C:\\Projects\\My Song\\Audio\\vocal.wav"));
    // Channel 1: a stock sample.
    event_var(e, 201, utf16(""));
    event_var(e, 196, utf16("%FLStudioFactoryData%\\Data\\Patches\\Packs\\Drums\\Kick.wav"));
    // Channel 2: a sample elsewhere on disk, used twice.
    event_var(e, 196, utf16("D:\\Samples\\snare.wav"));
    event_var(e, 196, utf16("D:\\Samples\\snare.wav"));

    // A native plugin, used twice.
    event_var(e, 201, utf16("Fruity Limiter"));
    event_var(e, 213, Buffer(52, 0x00));
    event_var(e, 201, utf16("Fruity Limiter"));
    event_var(e, 213, Buffer(52, 0x00));

    // Two wrapped plugins; the data is longer than 127 bytes, so the length
    // prefix takes two bytes.
    event_var(e, 201, utf16("Fruity Wrapper"));
    event_var(e, 212, Buffer(52, 0x00));
    event_var(e, 213, wrapper_data("Serum", "C:\\VST\\Serum_x64.dll", "Xfer Records"));
    event_var(e, 201, utf16("Fruity Wrapper"));
    event_var(e, 213, wrapper_data("Portal", "C:\\VST3\\Portal.vst3", "Output"));
    return e;
}

void test_modern_project() {
    const Buffer file = make_file(modern_events());
    std::vector<FlpEvent> events;
    const FlpResult r = read_flp(file, &events);
    CHECK(r.ok);
    CHECK(r.error.empty());
    CHECK(r.info.format == 0);
    CHECK(r.info.channel_count == 3);
    CHECK(r.info.ppq == 96);
    CHECK(r.info.fl_version == "25.1.6.4997");
    CHECK(r.info.title == "My Song");
    CHECK(r.info.data_path == "C:\\Projects\\My Song\\");
    CHECK(r.info.event_count == events.size());
    CHECK(events.size() == 21);

    CHECK(r.info.sample_paths.size() == 3);
    if (r.info.sample_paths.size() == 3) {
        CHECK(r.info.sample_paths[0] == "C:\\Projects\\My Song\\Audio\\vocal.wav");
        CHECK(r.info.sample_paths[2] == "D:\\Samples\\snare.wav");
    }

    CHECK(r.info.native_plugins == std::vector<std::string>{"Fruity Limiter"});
    CHECK(r.info.vst_plugins.size() == 2);
    if (r.info.vst_plugins.size() == 2) {
        CHECK((r.info.vst_plugins[0] == VstPlugin{"Serum", "Xfer Records", "C:\\VST\\Serum_x64.dll"}));
        CHECK(r.info.vst_plugins[1].name == "Portal");
        CHECK(r.info.vst_plugins[1].vendor == "Output");
    }
    CHECK(r.info.unparsed_vst_count == 0);
}

void test_non_ascii_text() {
    Buffer e;
    event_var(e, 199, ascii_z("21.2.3.4004"));
    // "Hé" + U+1F3B5 (a surrogate pair) -> UTF-8
    event_var(e, 194, utf16("H", {0x00E9, 0xD83C, 0xDFB5}));
    const FlpResult r = read_flp(make_file(e));
    CHECK(r.ok);
    CHECK(r.info.title == "H\xC3\xA9\xF0\x9F\x8E\xB5");

    // A lone surrogate becomes U+FFFD instead of breaking the string.
    Buffer lone;
    event_var(lone, 199, ascii_z("21.2.3.4004"));
    event_var(lone, 194, utf16("A", {0xD83C}));
    const FlpResult r2 = read_flp(make_file(lone));
    CHECK(r2.ok);
    CHECK(r2.info.title == "A\xEF\xBF\xBD");
}

void test_old_8bit_project() {
    Buffer e;
    event_var(e, 199, ascii_z("9.0.3"));
    event_var(e, 194, ascii_z("Old Tune"));
    event_var(e, 201, ascii_z("3x Osc"));
    event_var(e, 196, ascii_z("C:\\Samples\\clap.wav"));
    const FlpResult r = read_flp(make_file(e));
    CHECK(r.ok);
    CHECK(r.info.title == "Old Tune");
    CHECK(r.info.native_plugins == std::vector<std::string>{"3x Osc"});
    CHECK(r.info.sample_paths == std::vector<std::string>{"C:\\Samples\\clap.wav"});

    // 11.5 is the first version with UTF-16 text; 11.1 is still 8-bit.
    Buffer e115;
    event_var(e115, 199, ascii_z("11.5.0"));
    event_var(e115, 194, utf16("Wide"));
    CHECK(read_flp(make_file(e115)).info.title == "Wide");

    Buffer e111;
    event_var(e111, 199, ascii_z("11.1.0"));
    event_var(e111, 194, ascii_z("Narrow"));
    CHECK(read_flp(make_file(e111)).info.title == "Narrow");
}

void test_unreadable_files() {
    const Buffer good = make_file(modern_events());

    CHECK(!read_flp(Buffer{}).ok);
    CHECK(!read_flp(Buffer(10, 0)).ok);

    Buffer bad_magic = good;
    bad_magic[0] = 'X';
    CHECK(!read_flp(bad_magic).ok);

    Buffer bad_header_size = good;
    bad_header_size[4] = 7;
    CHECK(!read_flp(bad_header_size).ok);

    Buffer bad_data_magic = good;
    bad_data_magic[14] = 'X';
    CHECK(!read_flp(bad_data_magic).ok);

    // The data chunk claims more bytes than the file holds.
    Buffer truncated(good.begin(), good.end() - 5);
    CHECK(!read_flp(truncated).ok);

    // An event whose length runs past the end of the chunk.
    Buffer overrun_events;
    event_var(overrun_events, 199, ascii_z("25.1.6.4997"));
    overrun_events.push_back(213);
    overrun_events.push_back(100);  // claims 100 bytes, none follow
    CHECK(!read_flp(make_file(overrun_events)).ok);

    // A length prefix that never ends.
    Buffer endless;
    event_var(endless, 199, ascii_z("25.1.6.4997"));
    endless.push_back(213);
    for (int i = 0; i < 8; ++i) endless.push_back(0xFF);
    CHECK(!read_flp(make_file(endless)).ok);

    // A fixed-size event cut short.
    Buffer cut_fixed;
    event_var(cut_fixed, 199, ascii_z("25.1.6.4997"));
    cut_fixed.push_back(128);
    cut_fixed.push_back(1);
    CHECK(!read_flp(make_file(cut_fixed)).ok);

    // No version event.
    Buffer no_version;
    event_var(no_version, 194, utf16("Song"));
    CHECK(!read_flp(make_file(no_version)).ok);

    // A version that is not a number.
    Buffer odd_version;
    event_var(odd_version, 199, ascii_z("beta"));
    CHECK(!read_flp(make_file(odd_version)).ok);

    // UTF-16 text with an odd number of bytes.
    Buffer odd_text;
    event_var(odd_text, 199, ascii_z("25.1.6.4997"));
    event_var(odd_text, 194, Buffer{'A', 0, 'B'});
    const FlpResult r = read_flp(make_file(odd_text));
    CHECK(!r.ok);
    CHECK(!r.error.empty());
}

void test_trailing_bytes_are_ignored() {
    Buffer file = make_file(modern_events());
    file.push_back(0xAA);
    file.push_back(0xBB);
    CHECK(read_flp(file).ok);
}

void test_unparsed_wrapper_is_counted() {
    Buffer e;
    event_var(e, 199, ascii_z("25.1.6.4997"));
    event_var(e, 201, utf16("Fruity Wrapper"));
    event_var(e, 213, Buffer{1, 2, 3});  // too short for a type
    event_var(e, 201, utf16("Fruity Wrapper"));
    Buffer broken = wrapper_data("Serum", "x", "y");
    broken.resize(broken.size() - 10);  // last sub-event overruns
    event_var(e, 213, broken);
    event_var(e, 201, utf16("Fruity Wrapper"));
    Buffer nameless;
    put_u32(nameless, 12);
    put_u32(nameless, 55);
    put_u64(nameless, 3);
    put_bytes(nameless, "abc");
    event_var(e, 213, nameless);  // well-formed but no name
    const FlpResult r = read_flp(make_file(e));
    CHECK(r.ok);
    CHECK(r.info.vst_plugins.empty());
    CHECK(r.info.unparsed_vst_count == 3);
}

void test_classify_sample() {
    const std::string folder = "C:\\Users\\me\\Documents\\Henosis Projects\\My Song";
    CHECK(classify_sample("C:\\Users\\me\\Documents\\Henosis Projects\\My Song\\Audio\\a.wav", folder) ==
          SampleLocation::InsideProject);
    // Case and separator differences do not matter on Windows.
    CHECK(classify_sample("c:/users/ME/documents/henosis projects/my song/audio/a.wav", folder) ==
          SampleLocation::InsideProject);
    CHECK(classify_sample("C:\\Users\\me\\Documents\\Henosis Projects\\My Song", folder + "\\") ==
          SampleLocation::Outside);
    // A sibling folder that merely starts with the same name is outside.
    CHECK(classify_sample("C:\\Users\\me\\Documents\\Henosis Projects\\My Song 2\\a.wav", folder) ==
          SampleLocation::Outside);
    CHECK(classify_sample("D:\\Samples\\snare.wav", folder) == SampleLocation::Outside);
    CHECK(classify_sample("%FLStudioFactoryData%\\Data\\Patches\\Kick.wav", folder) ==
          SampleLocation::Factory);
    CHECK(classify_sample("D:\\Samples\\snare.wav", "") == SampleLocation::Outside);
}

}  // namespace

int main() {
    test_modern_project();
    test_non_ascii_text();
    test_old_8bit_project();
    test_unreadable_files();
    test_trailing_bytes_are_ignored();
    test_unparsed_wrapper_is_counted();
    test_classify_sample();

    if (failures == 0) {
        std::printf("all FLP reader tests passed\n");
        return 0;
    }
    std::printf("%d check(s) failed\n", failures);
    return 1;
}
