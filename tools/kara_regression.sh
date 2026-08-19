#!/bin/bash

# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# kara_regression.sh runs the fork's root-required container regression
# suite (waves 01-06) against a bazel-built runsc.
#
# The container test binaries need two things bazel's sandbox does not
# provide: root (sandboxes, cgroups, PID namespaces) and a runsc binary in
# a "runfiles-like" layout - testutil.ConfigureExePath runs BEFORE
# flag.Parse, so the -runsc flag never applies outside bazel; it searches
# the CWD for a path containing a "_main" element (FindFile("runsc/runsc")).
# The validated invocation (see .wip/notes-wave02.md) is therefore: build a
# STATIC runsc, symlink it under <tmp>/_main/runsc/runsc, cd into that tree
# and run the compiled .test binary from there as root.
#
# Usage:
#   tools/kara_regression.sh [-p platforms] [-r test-regex] [-t tmpdir]
#     -p platforms   comma-separated platform list passed to the container
#                    tests via their -test_platforms flag (default: systrap;
#                    empty makes the tests use every available platform)
#     -r test-regex  -test.run filter for the container test binary
#                    (default: the fork-added regression subset)
#     -t tmpdir      scratch directory (default: mktemp)
#
# Environment: bazel (or bazelisk) on PATH; sudo for the test run itself.

set -euo pipefail

PLATFORMS="systrap"
CONTAINER_RUN='TestCheckpointCleansImageWhenSandboxDies|TestCheckpointRestoreBackToBack|TestCheckpointRestoreLongSleep|TestCheckpointRestorePassFDDonation|TestCheckpointRestoreSignalHandlerRead|TestPortForwardDialProbe|TestSignalUserspaceSpinTask|TestStateAfterInitKilled'
TMPDIR_ARG=""

while getopts "p:r:t:" opt; do
  case "${opt}" in
    p) PLATFORMS="${OPTARG}" ;;
    r) CONTAINER_RUN="${OPTARG}" ;;
    t) TMPDIR_ARG="${OPTARG}" ;;
    *) echo "usage: $0 [-p platforms] [-r test-regex] [-t tmpdir]" >&2; exit 2 ;;
  esac
done

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

WORK="${TMPDIR_ARG:-$(mktemp -d /tmp/kara-regression.XXXXXX)}"
mkdir -p "${WORK}"
echo "==> workspace: ${WORK}"

echo "==> building static runsc and test binaries (bazel)"
bazel build //runsc:runsc //runsc/container:container_test //runsc/library:library_test

echo "==> laying out the _main runfiles shape"
mkdir -p "${WORK}/rt/_main/runsc"
cp -f bazel-bin/runsc/runsc_/runsc "${WORK}/rt/_main/runsc/runsc"
cp -f bazel-bin/runsc/container/container_test_/container_test "${WORK}/container.test"
cp -f bazel-bin/runsc/library/library_test_/library_test "${WORK}/library.test"
chmod +x "${WORK}"/container.test "${WORK}"/library.test

mkdir -p "${WORK}/tmp"
cd "${WORK}/rt/_main/runsc"

echo "==> container regression subset (platforms: ${PLATFORMS})"
sudo env TEST_TMPDIR="${WORK}/tmp" \
  "${WORK}/container.test" -test.v -test.timeout 3600s \
  -test_platforms="${PLATFORMS}" -test.run "${CONTAINER_RUN}"

echo "==> library reference embedder suite (platforms: ${PLATFORMS})"
sudo env TEST_TMPDIR="${WORK}/tmp" \
  "${WORK}/library.test" -test.v -test.timeout 1200s

echo "==> PASS"
