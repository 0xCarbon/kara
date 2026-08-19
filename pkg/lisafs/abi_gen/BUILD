load("//tools:defs.bzl", "go_binary", "go_library")

package(
    default_applicable_licenses = ["//:license"],
    licenses = ["notice"],
)

go_library(
    name = "abi_gen",
    srcs = ["main.go"],
    importpath = "gvisor.dev/gvisor/pkg/lisafs/abi_gen",
    visibility = ["//:sandbox"],
)

go_binary(
    name = "abi_gen_bin",
    embed = [":abi_gen"],
    visibility = ["//:sandbox"],
)
