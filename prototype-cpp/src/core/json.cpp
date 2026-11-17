#include "core/json.h"

#include <charconv>
#include <cstdlib>
#include <utility>

#include "core/path_rules.h"

namespace henosis::core {
namespace {

constexpr int kMaxDepth = 32;

void append_utf8(std::string& out, std::uint32_t cp) {
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

class Parser {
public:
    explicit Parser(std::string_view text) : text_(text) {}

    bool document(Json& out) {
        skip_space();
        if (!value(out, 0)) return false;
        skip_space();
        return pos_ == text_.size();
    }

private:
    void skip_space() {
        while (pos_ < text_.size()) {
            const char c = text_[pos_];
            if (c != ' ' && c != '\t' && c != '\n' && c != '\r') break;
            ++pos_;
        }
    }

    bool literal(std::string_view word) {
        if (text_.compare(pos_, word.size(), word) != 0) return false;
        pos_ += word.size();
        return true;
    }

    bool hex4(std::uint32_t& out) {
        if (text_.size() - pos_ < 4) return false;
        out = 0;
        for (int i = 0; i < 4; ++i) {
            const char c = text_[pos_++];
            out <<= 4;
            if (c >= '0' && c <= '9') {
                out |= static_cast<std::uint32_t>(c - '0');
            } else if (c >= 'a' && c <= 'f') {
                out |= static_cast<std::uint32_t>(c - 'a' + 10);
            } else if (c >= 'A' && c <= 'F') {
                out |= static_cast<std::uint32_t>(c - 'A' + 10);
            } else {
                return false;
            }
        }
        return true;
    }

    bool string(std::string& out) {
        if (pos_ >= text_.size() || text_[pos_] != '"') return false;
        ++pos_;
        out.clear();
        for (;;) {
            if (pos_ >= text_.size()) return false;
            const char c = text_[pos_++];
            if (c == '"') break;
            if (static_cast<unsigned char>(c) < 0x20) return false;
            if (c != '\\') {
                out.push_back(c);
                continue;
            }
            if (pos_ >= text_.size()) return false;
            const char escape = text_[pos_++];
            switch (escape) {
                case '"':
                    out.push_back('"');
                    break;
                case '\\':
                    out.push_back('\\');
                    break;
                case '/':
                    out.push_back('/');
                    break;
                case 'b':
                    out.push_back('\b');
                    break;
                case 'f':
                    out.push_back('\f');
                    break;
                case 'n':
                    out.push_back('\n');
                    break;
                case 'r':
                    out.push_back('\r');
                    break;
                case 't':
                    out.push_back('\t');
                    break;
                case 'u': {
                    std::uint32_t cp = 0;
                    if (!hex4(cp)) return false;
                    if (cp >= 0xD800 && cp <= 0xDBFF) {
                        std::uint32_t low = 0;
                        if (!literal("\\u") || !hex4(low) || low < 0xDC00 || low > 0xDFFF) {
                            return false;
                        }
                        cp = 0x10000 + ((cp - 0xD800) << 10) + (low - 0xDC00);
                    } else if (cp >= 0xDC00 && cp <= 0xDFFF) {
                        return false;
                    }
                    append_utf8(out, cp);
                    break;
                }
                default:
                    return false;
            }
        }
        return is_valid_utf8(out);
    }

    bool number(Json& out) {
        const std::size_t start = pos_;
        if (pos_ < text_.size() && text_[pos_] == '-') ++pos_;
        const std::size_t digits_start = pos_;
        while (pos_ < text_.size() && text_[pos_] >= '0' && text_[pos_] <= '9') ++pos_;
        if (pos_ == digits_start) return false;
        if (pos_ - digits_start > 1 && text_[digits_start] == '0') return false;  // no leading zeros

        bool whole = true;
        if (pos_ < text_.size() && text_[pos_] == '.') {
            whole = false;
            ++pos_;
            const std::size_t fraction_start = pos_;
            while (pos_ < text_.size() && text_[pos_] >= '0' && text_[pos_] <= '9') ++pos_;
            if (pos_ == fraction_start) return false;
        }
        if (pos_ < text_.size() && (text_[pos_] == 'e' || text_[pos_] == 'E')) {
            whole = false;
            ++pos_;
            if (pos_ < text_.size() && (text_[pos_] == '+' || text_[pos_] == '-')) ++pos_;
            const std::size_t exponent_start = pos_;
            while (pos_ < text_.size() && text_[pos_] >= '0' && text_[pos_] <= '9') ++pos_;
            if (pos_ == exponent_start) return false;
        }

        const std::string token(text_.substr(start, pos_ - start));
        out.type = Json::Type::Number;
        out.number = std::strtod(token.c_str(), nullptr);
        if (whole && token[0] != '-') {
            std::uint64_t value = 0;
            const auto [end, ec] = std::from_chars(token.data(), token.data() + token.size(), value);
            if (ec == std::errc() && end == token.data() + token.size()) {
                out.is_integer = true;
                out.integer = value;
            }
        }
        return true;
    }

    bool value(Json& out, int depth) {
        if (depth > kMaxDepth || pos_ >= text_.size()) return false;
        const char c = text_[pos_];
        if (c == '{') {
            ++pos_;
            out.type = Json::Type::Object;
            skip_space();
            if (pos_ < text_.size() && text_[pos_] == '}') {
                ++pos_;
                return true;
            }
            for (;;) {
                skip_space();
                std::string key;
                if (!string(key)) return false;
                skip_space();
                if (pos_ >= text_.size() || text_[pos_] != ':') return false;
                ++pos_;
                skip_space();
                Json member;
                if (!value(member, depth + 1)) return false;
                out.object_keys.push_back(std::move(key));
                out.object_values.push_back(std::move(member));
                skip_space();
                if (pos_ >= text_.size()) return false;
                if (text_[pos_] == ',') {
                    ++pos_;
                    continue;
                }
                if (text_[pos_] == '}') {
                    ++pos_;
                    return true;
                }
                return false;
            }
        }
        if (c == '[') {
            ++pos_;
            out.type = Json::Type::Array;
            skip_space();
            if (pos_ < text_.size() && text_[pos_] == ']') {
                ++pos_;
                return true;
            }
            for (;;) {
                skip_space();
                Json element;
                if (!value(element, depth + 1)) return false;
                out.array.push_back(std::move(element));
                skip_space();
                if (pos_ >= text_.size()) return false;
                if (text_[pos_] == ',') {
                    ++pos_;
                    continue;
                }
                if (text_[pos_] == ']') {
                    ++pos_;
                    return true;
                }
                return false;
            }
        }
        if (c == '"') {
            out.type = Json::Type::String;
            return string(out.string);
        }
        if (c == 't') {
            out.type = Json::Type::Bool;
            out.boolean = true;
            return literal("true");
        }
        if (c == 'f') {
            out.type = Json::Type::Bool;
            out.boolean = false;
            return literal("false");
        }
        if (c == 'n') {
            out.type = Json::Type::Null;
            return literal("null");
        }
        return number(out);
    }

    std::string_view text_;
    std::size_t pos_ = 0;
};

}  // namespace

const Json* Json::get(std::string_view key) const {
    if (type != Type::Object) return nullptr;
    for (std::size_t i = 0; i < object_keys.size(); ++i) {
        if (object_keys[i] == key) return &object_values[i];
    }
    return nullptr;
}

std::optional<std::string> Json::string_at(std::string_view key) const {
    const Json* member = get(key);
    if (!member || member->type != Type::String) return std::nullopt;
    return member->string;
}

std::optional<std::uint64_t> Json::integer_at(std::string_view key) const {
    const Json* member = get(key);
    if (!member || member->type != Type::Number || !member->is_integer) return std::nullopt;
    return member->integer;
}

std::optional<bool> Json::bool_at(std::string_view key) const {
    const Json* member = get(key);
    if (!member || member->type != Type::Bool) return std::nullopt;
    return member->boolean;
}

std::optional<Json> parse_json(std::string_view text) {
    Json root;
    Parser parser(text);
    if (!parser.document(root)) return std::nullopt;
    return root;
}

std::string json_string(std::string_view text) {
    static constexpr char kDigits[] = "0123456789abcdef";
    std::string out;
    out.reserve(text.size() + 2);
    out.push_back('"');
    for (const char c : text) {
        const auto byte = static_cast<unsigned char>(c);
        if (c == '"') {
            out += "\\\"";
        } else if (c == '\\') {
            out += "\\\\";
        } else if (byte < 0x20) {
            out += "\\u00";
            out.push_back(kDigits[byte >> 4]);
            out.push_back(kDigits[byte & 0x0F]);
        } else {
            out.push_back(c);
        }
    }
    out.push_back('"');
    return out;
}

}  // namespace henosis::core
