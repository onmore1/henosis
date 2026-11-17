#include "core/sha256.h"

#include <algorithm>
#include <cstring>
#include <fstream>
#include <vector>

namespace henosis::core {
namespace {

constexpr std::array<std::uint32_t, 64> kRound = {
    0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
    0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
    0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
    0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
    0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
    0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
    0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
    0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
};

constexpr std::uint32_t rotr(std::uint32_t x, int n) { return (x >> n) | (x << (32 - n)); }

constexpr std::size_t kFileChunk = 1024 * 1024;

}  // namespace

Sha256::Sha256()
    : state_{0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
             0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19} {}

void Sha256::compress(const std::uint8_t* block) {
    std::array<std::uint32_t, 64> w{};
    for (std::size_t i = 0; i < 16; ++i) {
        w[i] = (static_cast<std::uint32_t>(block[4 * i]) << 24) |
               (static_cast<std::uint32_t>(block[4 * i + 1]) << 16) |
               (static_cast<std::uint32_t>(block[4 * i + 2]) << 8) |
               static_cast<std::uint32_t>(block[4 * i + 3]);
    }
    for (std::size_t i = 16; i < 64; ++i) {
        const std::uint32_t s0 = rotr(w[i - 15], 7) ^ rotr(w[i - 15], 18) ^ (w[i - 15] >> 3);
        const std::uint32_t s1 = rotr(w[i - 2], 17) ^ rotr(w[i - 2], 19) ^ (w[i - 2] >> 10);
        w[i] = w[i - 16] + s0 + w[i - 7] + s1;
    }

    std::uint32_t a = state_[0], b = state_[1], c = state_[2], d = state_[3];
    std::uint32_t e = state_[4], f = state_[5], g = state_[6], h = state_[7];
    for (std::size_t i = 0; i < 64; ++i) {
        const std::uint32_t s1 = rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25);
        const std::uint32_t ch = (e & f) ^ (~e & g);
        const std::uint32_t t1 = h + s1 + ch + kRound[i] + w[i];
        const std::uint32_t s0 = rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22);
        const std::uint32_t maj = (a & b) ^ (a & c) ^ (b & c);
        const std::uint32_t t2 = s0 + maj;
        h = g;
        g = f;
        f = e;
        e = d + t1;
        d = c;
        c = b;
        b = a;
        a = t1 + t2;
    }
    state_[0] += a;
    state_[1] += b;
    state_[2] += c;
    state_[3] += d;
    state_[4] += e;
    state_[5] += f;
    state_[6] += g;
    state_[7] += h;
}

void Sha256::update(std::span<const std::uint8_t> data) {
    total_ += data.size();
    std::size_t offset = 0;

    if (buffered_ > 0) {
        const std::size_t take = std::min(data.size(), buffer_.size() - buffered_);
        std::memcpy(buffer_.data() + buffered_, data.data(), take);
        buffered_ += take;
        offset = take;
        if (buffered_ < buffer_.size()) return;
        compress(buffer_.data());
        buffered_ = 0;
    }
    while (data.size() - offset >= 64) {
        compress(data.data() + offset);
        offset += 64;
    }
    const std::size_t rest = data.size() - offset;
    if (rest > 0) {
        std::memcpy(buffer_.data(), data.data() + offset, rest);
        buffered_ = rest;
    }
}

std::array<std::uint8_t, 32> Sha256::finish() {
    const std::uint64_t bits = total_ * 8;

    buffer_[buffered_++] = 0x80;
    if (buffered_ > 56) {
        std::fill(buffer_.begin() + static_cast<std::ptrdiff_t>(buffered_), buffer_.end(),
                  std::uint8_t{0});
        compress(buffer_.data());
        buffered_ = 0;
    }
    std::fill(buffer_.begin() + static_cast<std::ptrdiff_t>(buffered_), buffer_.begin() + 56,
              std::uint8_t{0});
    for (std::size_t i = 0; i < 8; ++i) {
        buffer_[56 + i] = static_cast<std::uint8_t>(bits >> (8 * (7 - i)));
    }
    compress(buffer_.data());

    std::array<std::uint8_t, 32> digest{};
    for (std::size_t i = 0; i < 8; ++i) {
        digest[4 * i] = static_cast<std::uint8_t>(state_[i] >> 24);
        digest[4 * i + 1] = static_cast<std::uint8_t>(state_[i] >> 16);
        digest[4 * i + 2] = static_cast<std::uint8_t>(state_[i] >> 8);
        digest[4 * i + 3] = static_cast<std::uint8_t>(state_[i]);
    }
    return digest;
}

std::string to_hex(std::span<const std::uint8_t> bytes) {
    static constexpr char kDigits[] = "0123456789abcdef";
    std::string out;
    out.reserve(bytes.size() * 2);
    for (const std::uint8_t byte : bytes) {
        out.push_back(kDigits[byte >> 4]);
        out.push_back(kDigits[byte & 0x0F]);
    }
    return out;
}

std::string sha256_hex(std::string_view data) {
    Sha256 hasher;
    hasher.update({reinterpret_cast<const std::uint8_t*>(data.data()), data.size()});
    return to_hex(hasher.finish());
}

std::optional<FileHash> sha256_file(const std::filesystem::path& path) {
    std::ifstream file(path, std::ios::binary);
    if (!file) return std::nullopt;

    Sha256 hasher;
    std::vector<std::uint8_t> chunk(kFileChunk);
    FileHash result;
    while (file) {
        file.read(reinterpret_cast<char*>(chunk.data()), static_cast<std::streamsize>(chunk.size()));
        const std::streamsize got = file.gcount();
        if (got <= 0) break;
        hasher.update({chunk.data(), static_cast<std::size_t>(got)});
        result.size += static_cast<std::uint64_t>(got);
    }
    if (file.bad()) return std::nullopt;
    result.sha256 = to_hex(hasher.finish());
    return result;
}

bool is_sha256_hex(std::string_view text) {
    if (text.size() != 64) return false;
    return std::all_of(text.begin(), text.end(), [](char c) {
        return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f');
    });
}

}  // namespace henosis::core
