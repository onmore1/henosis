// SHA-256, used to name blobs and manifests by their content.
#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <filesystem>
#include <optional>
#include <span>
#include <string>
#include <string_view>

namespace henosis::core {

class Sha256 {
public:
    Sha256();
    void update(std::span<const std::uint8_t> data);
    // Returns the digest. Call once; the object must not be used afterwards.
    std::array<std::uint8_t, 32> finish();

private:
    void compress(const std::uint8_t* block);

    std::array<std::uint32_t, 8> state_;
    std::array<std::uint8_t, 64> buffer_{};
    std::size_t buffered_ = 0;
    std::uint64_t total_ = 0;
};

// 64 lowercase hex characters.
std::string to_hex(std::span<const std::uint8_t> bytes);
std::string sha256_hex(std::string_view data);

struct FileHash {
    std::string sha256;
    std::uint64_t size = 0;
};

// Hashes a file in 1 MiB chunks. Returns nothing when it cannot be read.
std::optional<FileHash> sha256_file(const std::filesystem::path& path);

bool is_sha256_hex(std::string_view text);

}  // namespace henosis::core
