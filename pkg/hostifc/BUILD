load("//tools:defs.bzl", "go_library", "go_test")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "hostifc",
    srcs = [
        "hostifc.go",
        "ipc_linux.go",
        "ipc_stub.go",
        "probes.go",
        "probes_linux.go",
        "probes_stub.go",
    ],
    visibility = ["//visibility:public"],
    deps = [
        "//pkg/fdchannel",
        "//pkg/flipcall",
        "//pkg/unet",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)

go_test(
    name = "hostifc_test",
    size = "small",
    srcs = ["hostifc_test.go"],
    library = ":hostifc",
    deps = [
        "//pkg/fdchannel",
        "//pkg/flipcall",
        "@org_golang_x_sys//unix:go_default_library",
    ],
)
