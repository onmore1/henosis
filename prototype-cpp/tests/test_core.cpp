// Unit tests for client/src/core: hashing, path rules, JSON, manifests and the
// three-way merge plan.

#include <algorithm>
#include <array>
#include <cstdint>
#include <optional>
#include <set>
#include <string>
#include <vector>

#include "check.h"
#include "core/json.h"
#include "core/manifest.h"
#include "core/merge.h"
#include "core/path_rules.h"
#include "core/sha256.h"
#include "core/util.h"

using namespace henosis::core;

namespace {

// --- SHA-256 ----------------------------------------------------------------

void test_sha256() {
    // Published test vectors (FIPS 180-4 examples).
    CHECK(sha256_hex("") == "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
    CHECK(sha256_hex("abc") == "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
    CHECK(sha256_hex("abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq") ==
          "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1");
    CHECK(sha256_hex(std::string(1'000'000, 'a')) ==
          "cdc76e5c9914fb9281a1c7e284d73e67f1809a48a497200e046d39ccc7112cd0");

    // Feeding the data in uneven pieces gives the same digest.
    std::string data;
    for (int i = 0; i < 5000; ++i) data.push_back(static_cast<char>((i * 31 + 7) & 0xFF));
    const std::string whole = sha256_hex(data);
    for (const std::size_t step : {1u, 3u, 55u, 56u, 63u, 64u, 65u, 119u, 1000u}) {
        Sha256 hasher;
        for (std::size_t at = 0; at < data.size(); at += step) {
            const std::size_t n = std::min(step, data.size() - at);
            hasher.update({reinterpret_cast<const std::uint8_t*>(data.data()) + at, n});
        }
        CHECK(to_hex(hasher.finish()) == whole);
    }

    // Padding boundaries: every length around one and two blocks.
    std::set<std::string> seen;
    for (std::size_t n = 50; n <= 130; ++n) seen.insert(sha256_hex(std::string(n, 'x')));
    CHECK(seen.size() == 81);

    CHECK(is_sha256_hex(whole));
    CHECK(!is_sha256_hex("abc"));
    CHECK(!is_sha256_hex(std::string(64, 'G')));
    CHECK(!is_sha256_hex(std::string(64, 'A')));  // upper case is not canonical
}

// --- Path rules ---------------------------------------------------------------

void test_path_rules() {
    CHECK(validate_path("My Song.flp") == PathError::None);
    CHECK(validate_path("Audio/vocal take 03.wav") == PathError::None);
    CHECK(validate_path("Audio/Malibu \xE2\x80\x94 Camargue.mp3") == PathError::None);
    CHECK(validate_path("a/b/c/d.txt") == PathError::None);
    CHECK(validate_path(".hidden") == PathError::None);
    CHECK(validate_path("console.txt") == PathError::None);  // only the exact device names
    CHECK(validate_path("com10.txt") == PathError::None);

    CHECK(validate_path("") == PathError::Empty);
    CHECK(validate_path("/abs.txt") == PathError::Absolute);
    CHECK(validate_path("a//b") == PathError::EmptySegment);
    CHECK(validate_path("a/") == PathError::EmptySegment);
    CHECK(validate_path("./a") == PathError::DotSegment);
    CHECK(validate_path("a/../b") == PathError::DotSegment);
    CHECK(validate_path("a\\b") == PathError::ForbiddenCharacter);
    CHECK(validate_path("C:/x") == PathError::ForbiddenCharacter);
    CHECK(validate_path("what?.wav") == PathError::ForbiddenCharacter);
    CHECK(validate_path("a|b") == PathError::ForbiddenCharacter);
    CHECK(validate_path("tab\there") == PathError::ControlCharacter);
    CHECK(validate_path("trailing /x") == PathError::TrailingSpaceOrDot);
    CHECK(validate_path("x/trailing.") == PathError::TrailingSpaceOrDot);
    CHECK(validate_path("CON") == PathError::ReservedName);
    CHECK(validate_path("Audio/nul.wav") == PathError::ReservedName);
    CHECK(validate_path("lpt1") == PathError::ReservedName);
    CHECK(validate_path("Com9.txt") == PathError::ReservedName);
    CHECK(validate_path("bad\xFF.wav") == PathError::InvalidUtf8);
    CHECK(validate_path("overlong\xC0\xAF") == PathError::InvalidUtf8);
    CHECK(validate_path(std::string(200, 'a')) == PathError::None);
    CHECK(validate_path(std::string(201, 'a')) == PathError::TooLong);

    // The limit counts characters, not bytes.
    std::string wide;
    for (int i = 0; i < 200; ++i) wide += "\xC3\xA9";
    CHECK(validate_path(wide) == PathError::None);

    CHECK(is_ignored(".henosis/base.json"));
    CHECK(is_ignored("Backup/eltan (overwritten at 19h26).flp"));
    CHECK(is_ignored("backups/x.flp"));
    CHECK(is_ignored("Audio/Backup/x.wav"));
    CHECK(is_ignored("eltan.flp~"));
    CHECK(is_ignored("Audio/render.TMP"));
    CHECK(is_ignored("x.part"));
    CHECK(is_ignored("Thumbs.db"));
    CHECK(is_ignored("Audio/desktop.ini"));
    CHECK(!is_ignored("eltan.flp"));
    CHECK(!is_ignored("Audio/Backup plan.wav"));
    CHECK(!is_ignored("Backups.flp"));

    CHECK(is_fl_save_in_progress("eltan.flp~"));
    CHECK(is_fl_save_in_progress("sub/New.FLP~"));
    CHECK(!is_fl_save_in_progress("eltan.flp"));
    CHECK(!is_fl_save_in_progress("notes.txt~"));
}

// --- JSON -----------------------------------------------------------------------

void test_json() {
    const auto doc = parse_json(
        " { \"a\" : 1, \"b\" : [true, false, null, -2.5e1], \"c\" : \"x\\n\\u00e9\\ud83c\\udfb5\","
        " \"big\": 18446744073709551615, \"nested\": {\"k\": \"v\"} } ");
    CHECK(doc.has_value());
    if (doc) {
        CHECK(doc->integer_at("a") == std::optional<std::uint64_t>(1));
        CHECK(doc->integer_at("big") == std::optional<std::uint64_t>(18446744073709551615ull));
        CHECK(doc->string_at("c") == std::optional<std::string>("x\n\xC3\xA9\xF0\x9F\x8E\xB5"));
        const Json* b = doc->get("b");
        CHECK(b && b->array.size() == 4);
        if (b && b->array.size() == 4) {
            CHECK(b->array[0].boolean);
            CHECK(b->array[2].type == Json::Type::Null);
            CHECK(b->array[3].number == -25.0);
            CHECK(!b->array[3].is_integer);
        }
        CHECK(doc->get("nested") && doc->get("nested")->string_at("k") == std::optional<std::string>("v"));
        CHECK(!doc->string_at("a"));
        CHECK(!doc->get("missing"));
    }

    for (const char* bad : {"", "{", "{\"a\":}", "[1,]", "{\"a\":1,}", "01", "1.", "nul", "\"abc",
                            "\"bad \\x escape\"", "{\"a\":1} extra", "\"raw\ncontrol\"",
                            "\"\\ud83c alone\"", "\"\xFF\"", "{'a':1}", "[1 2]"}) {
        CHECK(!parse_json(bad).has_value());
    }

    // Nesting deeper than the limit is refused, not a stack overflow.
    CHECK(!parse_json(std::string(5000, '[') + std::string(5000, ']')).has_value());

    CHECK(json_string("plain") == "\"plain\"");
    CHECK(json_string("q\"b\\n\n") == "\"q\\\"b\\\\n\\u000a\"");
    const std::string tricky = "Audio/\"odd\" \\ name \xE2\x80\x94 \x01.wav";
    const auto round = parse_json(json_string(tricky));
    CHECK(round && round->string == tricky);
}

// --- Manifest -------------------------------------------------------------------

const std::string kHashA(64, 'a');
const std::string kHashB(64, 'b');
const std::string kHashC(64, 'c');

void test_manifest() {
    Manifest m;
    m.files.push_back({"My Song.flp", kHashB, 482113});
    m.files.push_back({"Audio/vocal.wav", kHashA, 18422060});
    m.sort();
    CHECK(m.files[0].path == "Audio/vocal.wav");

    const std::string expected =
        "{\"format\":1,\"files\":[{\"path\":\"Audio/vocal.wav\",\"sha256\":\"" + kHashA +
        "\",\"size\":18422060},{\"path\":\"My Song.flp\",\"sha256\":\"" + kHashB +
        "\",\"size\":482113}]}";
    CHECK(m.to_json() == expected);
    CHECK(m.hash() == sha256_hex(expected));
    CHECK(validate_manifest(m).empty());
    CHECK(m.find("My Song.flp") && m.find("My Song.flp")->size == 482113);
    CHECK(!m.find("nope"));

    // Round trip, with entries out of order and unknown keys present.
    const std::string shuffled =
        "{\"extra\":{\"x\":[1,2]},\"files\":[{\"size\":482113,\"path\":\"My Song.flp\",\"sha256\":\"" +
        kHashB + "\",\"future\":true},{\"path\":\"Audio/vocal.wav\",\"sha256\":\"" + kHashA +
        "\",\"size\":18422060}],\"format\":1}";
    const ManifestParse parsed = parse_manifest(shuffled);
    CHECK(parsed.ok);
    CHECK(parsed.manifest == m);
    CHECK(parse_manifest(m.to_json()).manifest == m);

    CHECK(Manifest{}.to_json() == "{\"format\":1,\"files\":[]}");
    CHECK(parse_manifest("{\"format\":1,\"files\":[]}").ok);

    auto rejects = [](const std::string& json) { return !parse_manifest(json).ok; };
    CHECK(rejects("[]"));
    CHECK(rejects("{\"format\":2,\"files\":[]}"));
    CHECK(rejects("{\"format\":1}"));
    CHECK(rejects("{\"format\":1,\"files\":[{\"path\":\"a\",\"sha256\":\"" + kHashA + "\"}]}"));
    CHECK(rejects("{\"format\":1,\"files\":[{\"path\":\"a\",\"sha256\":\"short\",\"size\":1}]}"));
    CHECK(rejects("{\"format\":1,\"files\":[{\"path\":\"../a\",\"sha256\":\"" + kHashA + "\",\"size\":1}]}"));
    CHECK(rejects("{\"format\":1,\"files\":[{\"path\":\"a\",\"sha256\":\"" + kHashA +
                  "\",\"size\":1},{\"path\":\"a\",\"sha256\":\"" + kHashB + "\",\"size\":1}]}"));
    CHECK(rejects("{\"format\":1,\"files\":[{\"path\":\"Kick.wav\",\"sha256\":\"" + kHashA +
                  "\",\"size\":1},{\"path\":\"kick.WAV\",\"sha256\":\"" + kHashB + "\",\"size\":1}]}"));
    CHECK(rejects("{\"format\":1,\"files\":[{\"path\":\".henosis/x\",\"sha256\":\"" + kHashA + "\",\"size\":1}]}"));
    CHECK(rejects("{\"format\":1,\"files\":[{\"path\":\"a\",\"sha256\":\"" + kHashA + "\",\"size\":-1}]}"));

    Manifest unsorted;
    unsorted.files.push_back({"b", kHashA, 1});
    unsorted.files.push_back({"a", kHashA, 1});
    CHECK(!validate_manifest(unsorted).empty());
}

// --- Diff and three-way merge -----------------------------------------------------

Manifest one(const char* path, const std::string& hash) {
    Manifest m;
    if (!hash.empty()) m.files.push_back({path, hash, 1});
    return m;
}

void test_diff() {
    Manifest from;
    from.files = {{"a", kHashA, 1}, {"b", kHashA, 1}, {"c", kHashA, 1}};
    Manifest to;
    to.files = {{"b", kHashB, 1}, {"c", kHashA, 1}, {"d", kHashC, 1}};
    const Diff d = diff(from, to);
    CHECK(d.removed == std::vector<std::string>{"a"});
    CHECK(d.changed.size() == 1 && d.changed[0].path == "b" && d.changed[0].sha256 == kHashB);
    CHECK(d.added.size() == 1 && d.added[0].path == "d");
    CHECK(diff(from, from).empty());
    CHECK(diff(Manifest{}, from).added.size() == 3);
    CHECK(diff(from, Manifest{}).removed.size() == 3);
}

void test_merge_rule_table() {
    // The four rows of the table in the docs.
    auto action = [](const std::string& b, const std::string& l, const std::string& h) {
        const MergePlan plan = plan_merge(one("f", b), one("f", l), one("f", h));
        return plan.steps.empty() ? std::optional<MergeAction>() : plan.steps[0].action;
    };
    CHECK(!action(kHashA, kHashA, kHashA));                             // nothing changed
    CHECK(action(kHashA, kHashA, kHashB) == MergeAction::TakeHead);     // only the head changed
    CHECK(action(kHashA, kHashA, "") == MergeAction::DeleteLocal);      // head deleted it
    CHECK(action("", "", kHashB) == MergeAction::TakeHead);             // head created it
    CHECK(action(kHashA, kHashB, kHashA) == MergeAction::KeepLocal);    // only local changed
    CHECK(action(kHashA, "", kHashA) == MergeAction::KeepLocal);        // local deleted it
    CHECK(action("", kHashB, "") == MergeAction::KeepLocal);            // local created it
    CHECK(!action(kHashA, kHashB, kHashB));                             // both made the same edit
    CHECK(!action(kHashA, "", ""));                                     // both deleted it
    CHECK(action(kHashA, kHashB, kHashC) == MergeAction::Conflict);     // different edits
    CHECK(action("", kHashB, kHashC) == MergeAction::Conflict);         // both created it
    CHECK(action(kHashA, kHashB, "") == MergeAction::Conflict);         // edited here, deleted there
    CHECK(action(kHashA, "", kHashC) == MergeAction::TakeHead);         // deleted here, edited there

    const MergePlan conflict = plan_merge(one("f", kHashA), one("f", kHashB), one("f", kHashC));
    CHECK(conflict.conflicts == 1 && conflict.has_draft);
    CHECK(conflict.steps[0].local_exists && conflict.steps[0].head && conflict.steps[0].head->sha256 == kHashC);
}

void test_merge_never_loses_local_work() {
    // Every combination of absent / A / B / C for base, local and head.
    const std::array<std::string, 4> values = {"", kHashA, kHashB, kHashC};
    int combinations = 0;
    for (const std::string& b : values) {
        for (const std::string& l : values) {
            for (const std::string& h : values) {
                ++combinations;
                const MergePlan plan = plan_merge(one("f", b), one("f", l), one("f", h));
                CHECK(plan.steps.size() <= 1);
                if (l == h) {
                    CHECK(plan.steps.empty());
                    continue;
                }
                CHECK(plan.steps.size() == 1);
                if (plan.steps.size() != 1) continue;
                const MergeAction a = plan.steps[0].action;

                // Local content that is not in the history (it differs from the
                // base) and differs from the head must survive on disk.
                const bool local_work = !l.empty() && l != b;
                if (local_work) CHECK(a == MergeAction::KeepLocal || a == MergeAction::Conflict);

                // The local file is only overwritten or removed when it is
                // unchanged since the base, or already gone.
                if (a == MergeAction::TakeHead || a == MergeAction::DeleteLocal) {
                    CHECK(l == b || l.empty());
                }
                // An untouched local file always follows the head.
                if (l == b) CHECK(a == MergeAction::TakeHead || a == MergeAction::DeleteLocal);
                if (a == MergeAction::DeleteLocal) CHECK(h.empty() && !l.empty());
                if (a == MergeAction::TakeHead) CHECK(!h.empty());
            }
        }
    }
    CHECK(combinations == 64);
}

void test_merge_many_paths() {
    Manifest base;
    base.files = {{"a", kHashA, 1}, {"b", kHashA, 1}, {"c", kHashA, 1}, {"d", kHashA, 1}};
    Manifest local;  // edits a, deletes b, adds e
    local.files = {{"a", kHashB, 1}, {"c", kHashA, 1}, {"d", kHashA, 1}, {"e", kHashC, 1}};
    Manifest head;   // edits c, deletes d, adds f
    head.files = {{"a", kHashA, 1}, {"b", kHashA, 1}, {"c", kHashB, 1}, {"f", kHashC, 1}};

    const MergePlan plan = plan_merge(base, local, head);
    CHECK(plan.conflicts == 0);
    CHECK(plan.has_draft);
    std::set<std::string> seen;
    for (const MergeStep& step : plan.steps) {
        seen.insert(step.path);
        if (step.path == "a" || step.path == "b" || step.path == "e") CHECK(step.action == MergeAction::KeepLocal);
        if (step.path == "c" || step.path == "f") CHECK(step.action == MergeAction::TakeHead);
        if (step.path == "d") CHECK(step.action == MergeAction::DeleteLocal);
    }
    CHECK(seen == (std::set<std::string>{"a", "b", "c", "d", "e", "f"}));
}

void test_conflict_names() {
    std::set<std::string> used;
    auto taken = [&](std::string_view name) { return used.contains(std::string(name)); };

    CHECK(conflict_copy_name("My Song.flp", "vadym", "2026-11-15", taken) ==
          "My Song (conflict, vadym, 2026-11-15).flp");
    used.insert("My Song (conflict, vadym, 2026-11-15).flp");
    CHECK(conflict_copy_name("My Song.flp", "vadym", "2026-11-15", taken) ==
          "My Song (conflict, vadym, 2026-11-15 2).flp");
    used.insert("My Song (conflict, vadym, 2026-11-15 2).flp");
    CHECK(conflict_copy_name("My Song.flp", "vadym", "2026-11-15", taken) ==
          "My Song (conflict, vadym, 2026-11-15 3).flp");

    CHECK(conflict_copy_name("Audio/take.01.wav", "sam", "2026-01-02", taken) ==
          "Audio/take.01 (conflict, sam, 2026-01-02).wav");
    CHECK(conflict_copy_name("Audio/noext", "sam", "2026-01-02", taken) ==
          "Audio/noext (conflict, sam, 2026-01-02)");
    CHECK(conflict_copy_name(".config", "sam", "2026-01-02", taken) ==
          ".config (conflict, sam, 2026-01-02)");
    // A user name with characters that are not allowed in file names.
    const std::string odd = conflict_copy_name("a.txt", "we/ird:name?", "2026-01-02", taken);
    CHECK(odd == "a (conflict, we_ird_name_, 2026-01-02).txt");
    CHECK(validate_path(odd) == PathError::None);
}

void test_util() {
    const std::string id = random_uuid();
    CHECK(id.size() == 36 && id[8] == '-' && id[13] == '-' && id[14] == '4' && id[18] == '-' && id[23] == '-');
    CHECK(random_uuid() != id);
    const std::string now = now_rfc3339();
    CHECK(now.size() == 20 && now[4] == '-' && now[10] == 'T' && now.back() == 'Z');
    CHECK(today_date() == now.substr(0, 10));
    CHECK(path_to_utf8(path_from_utf8("Audio/Malibu \xE2\x80\x94 x.mp3")) == "Audio/Malibu \xE2\x80\x94 x.mp3");
}

}  // namespace

int main() {
    test_sha256();
    test_path_rules();
    test_json();
    test_manifest();
    test_diff();
    test_merge_rule_table();
    test_merge_never_loses_local_work();
    test_merge_many_paths();
    test_conflict_names();
    test_util();
    return henosis::test::finish("core");
}
