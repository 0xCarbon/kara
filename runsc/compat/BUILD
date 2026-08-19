load("//tools:defs.bzl", "go_library", "go_test")

go_library(
    name = "compat",
    srcs = ["compat.go"],
    visibility = ["//:sandbox"],
    deps = [
        "//pkg/cpuid",
        "//runsc/version",
    ],
)

go_test(
    name = "compat_test",
    size = "small",
    srcs = ["compat_test.go"],
    library = ":compat",
    deps = [
        "//pkg/cpuid",
        "//runsc/version",
    ],
)
