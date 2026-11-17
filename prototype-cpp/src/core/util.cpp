#include "core/util.h"

#include <array>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <fstream>
#include <iterator>
#include <random>
#include <system_error>

namespace henosis::core {

namespace {

// Formats the current UTC time with the C++20 calendar types. std::format is
// avoided on purpose: GCC only ships it from version 13.
std::string format_now(bool with_time) {
    const auto now = std::chrono::floor<std::chrono::seconds>(std::chrono::system_clock::now());
    const auto day = std::chrono::floor<std::chrono::days>(now);
    const std::chrono::year_month_day date{day};
    const std::chrono::hh_mm_ss<std::chrono::seconds> time{now - day};

    char buffer[32];
    if (with_time) {
        std::snprintf(buffer, sizeof buffer, "%04d-%02u-%02uT%02d:%02d:%02dZ",
                      static_cast<int>(date.year()), static_cast<unsigned>(date.month()),
                      static_cast<unsigned>(date.day()), static_cast<int>(time.hours().count()),
                      static_cast<int>(time.minutes().count()),
                      static_cast<int>(time.seconds().count()));
    } else {
        std::snprintf(buffer, sizeof buffer, "%04d-%02u-%02u", static_cast<int>(date.year()),
                      static_cast<unsigned>(date.month()), static_cast<unsigned>(date.day()));
    }
    return buffer;
}

}  // namespace

std::string now_rfc3339() { return format_now(true); }

std::string today_date() { return format_now(false); }

std::string random_uuid() {
    static thread_local std::mt19937_64 engine{std::random_device{}()};
    std::array<std::uint8_t, 16> bytes{};
    for (std::size_t i = 0; i < bytes.size(); i += 8) {
        const std::uint64_t word = engine();
        for (std::size_t k = 0; k < 8; ++k) bytes[i + k] = static_cast<std::uint8_t>(word >> (8 * k));
    }
    bytes[6] = static_cast<std::uint8_t>((bytes[6] & 0x0F) | 0x40);  // version 4
    bytes[8] = static_cast<std::uint8_t>((bytes[8] & 0x3F) | 0x80);  // RFC 4122 variant

    static constexpr char kDigits[] = "0123456789abcdef";
    std::string out;
    out.reserve(36);
    for (std::size_t i = 0; i < bytes.size(); ++i) {
        if (i == 4 || i == 6 || i == 8 || i == 10) out.push_back('-');
        out.push_back(kDigits[bytes[i] >> 4]);
        out.push_back(kDigits[bytes[i] & 0x0F]);
    }
    return out;
}

std::optional<std::string> read_text_file(const std::filesystem::path& path) {
    std::ifstream file(path, std::ios::binary);
    if (!file) return std::nullopt;
    std::string content((std::istreambuf_iterator<char>(file)), std::istreambuf_iterator<char>());
    if (file.bad()) return std::nullopt;
    return content;
}

bool write_file_atomic(const std::filesystem::path& path, std::string_view content) {
    std::error_code ec;
    if (path.has_parent_path()) std::filesystem::create_directories(path.parent_path(), ec);

    std::filesystem::path temp = path;
    temp += ".new";
    {
        std::ofstream file(temp, std::ios::binary | std::ios::trunc);
        if (!file) return false;
        file.write(content.data(), static_cast<std::streamsize>(content.size()));
        file.flush();
        if (!file) return false;
    }
    std::filesystem::rename(temp, path, ec);
    if (ec) {
        std::filesystem::remove(temp, ec);
        return false;
    }
    return true;
}

std::filesystem::path path_from_utf8(std::string_view utf8) {
    return std::filesystem::path(std::u8string(utf8.begin(), utf8.end()));
}

std::string path_to_utf8(const std::filesystem::path& path) {
    const std::u8string generic = path.generic_u8string();
    return std::string(generic.begin(), generic.end());
}

}  // namespace henosis::core
