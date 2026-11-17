#include "core/path_rules.h"

#include <array>
#include <cstdint>

namespace henosis::core {
namespace {

constexpr std::size_t kMaxPathCharacters = 200;

bool ends_with(std::string_view text, std::string_view suffix) {
    return text.size() >= suffix.size() &&
           text.compare(text.size() - suffix.size(), suffix.size(), suffix) == 0;
}

// Windows device names are reserved with or without an extension.
bool is_reserved_name(std::string_view segment) {
    const std::string stem = ascii_lower(segment.substr(0, segment.find('.')));
    static constexpr std::array<std::string_view, 4> kFixed = {"con", "prn", "aux", "nul"};
    for (const std::string_view name : kFixed) {
        if (stem == name) return true;
    }
    if (stem.size() == 4 && (stem.compare(0, 3, "com") == 0 || stem.compare(0, 3, "lpt") == 0)) {
        return stem[3] >= '1' && stem[3] <= '9';
    }
    return false;
}

template <typename Visitor>
void for_each_segment(std::string_view path, Visitor&& visit) {
    std::size_t start = 0;
    for (;;) {
        const std::size_t slash = path.find('/', start);
        const bool last = slash == std::string_view::npos;
        if (!visit(path.substr(start, last ? std::string_view::npos : slash - start), last)) return;
        if (last) return;
        start = slash + 1;
    }
}

}  // namespace

std::string ascii_lower(std::string_view text) {
    std::string out(text);
    for (char& c : out) {
        if (c >= 'A' && c <= 'Z') c = static_cast<char>(c - 'A' + 'a');
    }
    return out;
}

bool is_valid_utf8(std::string_view text) {
    std::size_t i = 0;
    while (i < text.size()) {
        const auto lead = static_cast<unsigned char>(text[i]);
        std::size_t extra = 0;
        std::uint32_t cp = 0;
        if (lead < 0x80) {
            ++i;
            continue;
        } else if ((lead & 0xE0) == 0xC0) {
            extra = 1;
            cp = lead & 0x1Fu;
        } else if ((lead & 0xF0) == 0xE0) {
            extra = 2;
            cp = lead & 0x0Fu;
        } else if ((lead & 0xF8) == 0xF0) {
            extra = 3;
            cp = lead & 0x07u;
        } else {
            return false;
        }
        if (text.size() - i <= extra) return false;
        for (std::size_t k = 1; k <= extra; ++k) {
            const auto byte = static_cast<unsigned char>(text[i + k]);
            if ((byte & 0xC0) != 0x80) return false;
            cp = (cp << 6) | (byte & 0x3Fu);
        }
        // Reject overlong forms, surrogates and values past U+10FFFF.
        static constexpr std::uint32_t kMinimum[] = {0, 0x80, 0x800, 0x10000};
        if (cp < kMinimum[extra] || cp > 0x10FFFF || (cp >= 0xD800 && cp <= 0xDFFF)) return false;
        i += extra + 1;
    }
    return true;
}

std::size_t count_code_points(std::string_view utf8) {
    std::size_t count = 0;
    for (const char c : utf8) {
        if ((static_cast<unsigned char>(c) & 0xC0) != 0x80) ++count;
    }
    return count;
}

PathError validate_path(std::string_view path) {
    if (path.empty()) return PathError::Empty;
    if (!is_valid_utf8(path)) return PathError::InvalidUtf8;
    if (count_code_points(path) > kMaxPathCharacters) return PathError::TooLong;
    if (path.front() == '/') return PathError::Absolute;

    PathError error = PathError::None;
    for_each_segment(path, [&](std::string_view segment, bool) {
        if (segment.empty()) {
            error = PathError::EmptySegment;
        } else if (segment == "." || segment == "..") {
            error = PathError::DotSegment;
        } else {
            for (const char c : segment) {
                const auto byte = static_cast<unsigned char>(c);
                if (byte < 0x20) {
                    error = PathError::ControlCharacter;
                    break;
                }
                if (c == '<' || c == '>' || c == ':' || c == '"' || c == '|' || c == '?' ||
                    c == '*' || c == '\\') {
                    error = PathError::ForbiddenCharacter;
                    break;
                }
            }
            if (error == PathError::None) {
                if (segment.back() == ' ' || segment.back() == '.') {
                    error = PathError::TrailingSpaceOrDot;
                } else if (is_reserved_name(segment)) {
                    error = PathError::ReservedName;
                }
            }
        }
        return error == PathError::None;
    });
    return error;
}

const char* describe(PathError error) {
    switch (error) {
        case PathError::None:
            return "valid";
        case PathError::Empty:
            return "the path is empty";
        case PathError::InvalidUtf8:
            return "the path is not valid UTF-8";
        case PathError::TooLong:
            return "the path is longer than 200 characters";
        case PathError::Absolute:
            return "the path is not relative";
        case PathError::EmptySegment:
            return "the path has an empty segment";
        case PathError::DotSegment:
            return "the path has a '.' or '..' segment";
        case PathError::ControlCharacter:
            return "the path has a control character";
        case PathError::ForbiddenCharacter:
            return "the path has a character Windows does not allow";
        case PathError::TrailingSpaceOrDot:
            return "a path segment ends in a space or a dot";
        case PathError::ReservedName:
            return "the path uses a reserved Windows device name";
    }
    return "unknown path error";
}

bool is_ignored(std::string_view relative_path) {
    bool ignored = false;
    for_each_segment(relative_path, [&](std::string_view segment, bool last) {
        const std::string lower = ascii_lower(segment);
        if (lower == ".henosis" || lower == "backups" || lower == "backup") {
            ignored = true;
        } else if (last) {
            ignored = ends_with(lower, ".tmp") || ends_with(lower, ".part") ||
                      ends_with(lower, "~") || lower == "thumbs.db" || lower == "desktop.ini" ||
                      lower == ".ds_store";
        }
        return !ignored;
    });
    return ignored;
}

bool is_fl_save_in_progress(std::string_view relative_path) {
    const std::size_t slash = relative_path.rfind('/');
    const std::string leaf = ascii_lower(
        slash == std::string_view::npos ? relative_path : relative_path.substr(slash + 1));
    return ends_with(leaf, ".flp~");
}

}  // namespace henosis::core
