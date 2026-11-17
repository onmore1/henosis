cmake_minimum_required(VERSION 3.21)
project(henosis_client LANGUAGES CXX)

set(CMAKE_CXX_STANDARD 20)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

if(MSVC)
  add_compile_options(/W4 /utf-8 /permissive-)
else()
  add_compile_options(-Wall -Wextra -Wpedantic)
endif()

# Core: hashing, path rules, manifests, three-way merge. No I/O beyond files.
add_library(henosis_core STATIC
  src/core/json.cpp
  src/core/manifest.cpp
  src/core/merge.cpp
  src/core/path_rules.cpp
  src/core/sha256.cpp
  src/core/util.cpp
)
target_include_directories(henosis_core PUBLIC src)

# Sync: scanner, the Remote interface, a folder-based remote, the engine.
add_library(henosis_sync STATIC
  src/sync/folder_remote.cpp
  src/sync/scanner.cpp
  src/sync/sync_engine.cpp
)
target_link_libraries(henosis_sync PUBLIC henosis_core)

# FL Studio specifics: the .flp reader (portable, no Qt).
add_library(henosis_fl STATIC
  src/fl/flp_reader.cpp
)
target_include_directories(henosis_fl PUBLIC src)

# Command-line tools.
add_executable(flpdump tools/flpdump/main.cpp)
target_link_libraries(flpdump PRIVATE henosis_fl)

add_executable(henosis-cli tools/henosis-cli/main.cpp)
target_link_libraries(henosis-cli PRIVATE henosis_sync)

enable_testing()

add_executable(test_flp_reader tests/test_flp_reader.cpp)
target_link_libraries(test_flp_reader PRIVATE henosis_fl)
add_test(NAME flp_reader COMMAND test_flp_reader)

add_executable(test_core tests/test_core.cpp)
target_link_libraries(test_core PRIVATE henosis_core)
add_test(NAME core COMMAND test_core)

add_executable(test_sync tests/test_sync.cpp)
target_link_libraries(test_sync PRIVATE henosis_sync)
add_test(NAME sync COMMAND test_sync)
