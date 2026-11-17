// Minimal test support: CHECK prints on failure and counts it.
#pragma once

#include <cstdio>

namespace henosis::test {

inline int& failures() {
    static int count = 0;
    return count;
}

inline int finish(const char* suite) {
    if (failures() == 0) {
        std::printf("all %s tests passed\n", suite);
        return 0;
    }
    std::printf("%d check(s) failed in %s\n", failures(), suite);
    return 1;
}

}  // namespace henosis::test

#define CHECK(condition)                                                        \
    do {                                                                        \
        if (!(condition)) {                                                     \
            ++henosis::test::failures();                                        \
            std::printf("FAILED %s:%d  %s\n", __FILE__, __LINE__, #condition);  \
        }                                                                       \
    } while (false)
