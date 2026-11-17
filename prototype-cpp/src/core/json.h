// A small strict JSON reader and the string escaping needed to write JSON.
// Enough for manifests, snapshot records and local state files.
#pragma once

#include <cstdint>
#include <optional>
#include <string>
#include <string_view>
#include <vector>

namespace henosis::core {

struct Json {
    enum class Type { Null, Bool, Number, String, Array, Object };

    Type type = Type::Null;
    bool boolean = false;
    double number = 0;
    // Set for a non-negative whole number that fits in 64 bits.
    bool is_integer = false;
    std::uint64_t integer = 0;
    std::string string;
    std::vector<Json> array;
    // An object's members, in document order: object_keys[i] names
    // object_values[i]. Two vectors, because a container of pairs would need
    // Json to be a complete type here.
    std::vector<std::string> object_keys;
    std::vector<Json> object_values;

    // Member lookup; nullptr when this is not an object or has no such key.
    const Json* get(std::string_view key) const;

    // Convenience readers that return nothing on a type mismatch.
    std::optional<std::string> string_at(std::string_view key) const;
    std::optional<std::uint64_t> integer_at(std::string_view key) const;
    std::optional<bool> bool_at(std::string_view key) const;
};

// Parses one JSON document. Rejects trailing data, invalid UTF-8, raw control
// characters in strings, and nesting deeper than 32 levels.
std::optional<Json> parse_json(std::string_view text);

// Returns `text` as a quoted JSON string. Non-ASCII characters are kept as
// UTF-8; quotes, backslashes and control characters are escaped.
std::string json_string(std::string_view text);

}  // namespace henosis::core
