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

# plain_go_import_test proves that pkg/lisafs, pkg/flipcall and pkg/fdchannel
# are importable by a plain Go module living outside the repository, without
# bazel, by:
#
#  1. unzipping the //:gopath archive (the same archive tools/go_branch.sh
#     publishes as the synthetic `go` branch),
#  2. reconstructing the gvisor.dev/gvisor module tree (go.mod + go.sum are
#     copied in, exactly like tools/build_cover.sh does),
#  3. creating a throwaway consumer module that imports all three packages and
#     references their exported symbols,
#  4. building and vetting the consumer with GOPROXY=off and a filesystem
#     replace directive, then vetting the three packages in-tree.
#
# The three golang.org/x module dependencies of that import closure
# (x/sys, x/exp, x/time; the only non-stdlib requirements of
# lisafs+flipcall+fdchannel) are resolved with additional replace directives
# pointing at copies from the ambient module cache, so the test never touches
# the network. If the cache is cold, run `go mod download` first (the CI job
# in .github/workflows/go.yml does this), or point
# PLAIN_GO_IMPORT_MODCACHE at a GOMODCACHE.
#
# Usage: plain_go_import_test.sh <gopath.zip> <go.mod> <go.sum>
# (bazel substitutes the $(location ...) expansions for the arguments.)

set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 <gopath.zip> <go.mod> <go.sum>" >&2
  exit 1
fi

gopath_zip=$(realpath "$1")
go_mod=$(realpath "$2")
go_sum=$(realpath "$3")

die() {
  echo "plain_go_import_test: $*" >&2
  exit 1
}

# Locate a Go toolchain. The bazel-built toolchain is not exported to test
# runfiles, so require `go` on PATH (forward it with --test_env=PATH when
# invoking bazel; CI pins it with actions/setup-go from go.mod).
go_bin=${GO:-$(command -v go || true)}
[[ -n "${go_bin}" ]] || die "no go toolchain on PATH; forward one with --test_env=PATH"
"$go_bin" version

work=$(mktemp -d)
# The module-cache copies are read-only; make them removable before rm.
trap 'chmod -R u+w "${work}" 2>/dev/null || true; rm -rf "${work}" || true' EXIT

# --- 1. Materialize the module tree exactly as the go branch does. ----------
mkdir -p "${work}/gopath"
unzip -q "${gopath_zip}" -d "${work}/gopath"
module_dir="${work}/gopath/src/gvisor.dev/gvisor"
[[ -d "${module_dir}/pkg/lisafs" ]] || die "gopath archive has no pkg/lisafs"
cp "${go_mod}" "${go_sum}" "${module_dir}/"

# --- 2. Copy the x/* module dependencies from the ambient module cache. -----
ambient_modcache=${PLAIN_GO_IMPORT_MODCACHE:-$("${go_bin}" env GOMODCACHE)}
go_version=$(awk '$1 == "go" { print $2; exit }' "${go_mod}")

x_modules=(golang.org/x/sys golang.org/x/exp golang.org/x/time)
replaces=()
for mod in "${x_modules[@]}"; do
  version=$(awk -v m="${mod}" '$1 == m { print $2; exit }' "${go_mod}")
  [[ -n "${version}" ]] || die "no requirement for ${mod} in go.mod"
  src="${ambient_modcache}/${mod}@${version}"
  if [[ ! -d "${src}" ]]; then
    die "${mod}@${version} not found in GOMODCACHE=${ambient_modcache}.
Run 'go mod download ${mod}' (with network) or set PLAIN_GO_IMPORT_MODCACHE."
  fi
  dst="${work}/mods/${mod}"
  mkdir -p "$(dirname "${dst}")"
  cp -r "${src}" "${dst}"
  # Filesystem replaces need the module's own go.mod, which the extracted
  # cache directory provides.
  [[ -f "${dst}/go.mod" ]] || die "${src} has no go.mod"
  replaces+=("${mod} => ${dst}")
done

# --- 3. Create the consumer module. ------------------------------------------
mkdir -p "${work}/consumer"
cat > "${work}/consumer/main.go" <<'EOF'
// Package main is a minimal out-of-tree consumer of the plain-Go bVisor
// surface. It exists to prove the packages build and vet outside bazel.
package main

import (
	"fmt"
	"os"

	"gvisor.dev/gvisor/pkg/fdchannel"
	"gvisor.dev/gvisor/pkg/flipcall"
	"gvisor.dev/gvisor/pkg/lisafs"
)

func main() {
	// Reference exported symbols of every package so the imports are real.
	mids := []lisafs.MID{lisafs.Error, lisafs.Mount, lisafs.Channel, lisafs.RenameAt2}
	fmt.Println("top MID", mids[len(mids)-1])

	fds, err := fdchannel.NewConnectedSockets()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fdchannel:", err)
		os.Exit(1)
	}
	ep := fdchannel.NewEndpoint(fds[0])
	ep.Destroy()

	var _ flipcall.EndpointSide = flipcall.ClientSide
	var _ flipcall.EndpointSide = flipcall.ServerSide
	_ = flipcall.PacketHeaderBytes
}
EOF

{
  echo "module example.com/plaingoconsumer"
  echo "go ${go_version}"
  echo
  echo "require gvisor.dev/gvisor v0.0.0-00010101000000-000000000000"
  echo
  echo "replace gvisor.dev/gvisor => ${module_dir}"
  for r in "${replaces[@]}"; do
    echo "replace ${r}"
  done
} > "${work}/consumer/go.mod"

# --- 4. Build + vet, fully offline. ------------------------------------------
export HOME="${work}"
export GOPATH="${work}/gopath"
export GOCACHE="${work}/gocache"
export GOMODCACHE="${work}/gocache/mod" # unused: everything is a filesystem replace
export GOPROXY=off
export GOFLAGS=-mod=mod
export GOTOOLCHAIN=local

echo "== consumer go build (out-of-tree, GOPROXY=off) =="
(cd "${work}/consumer" && "${go_bin}" build ./...)
echo "== consumer go vet =="
(cd "${work}/consumer" && "${go_bin}" vet ./...)
# Vet the packages inside the materialized tree too (a stronger check than
# the consumer alone: it type-checks the real sources). Redirect the x/*
# requirements at the local copies so this also works with GOPROXY=off.
{
  echo
  for r in "${replaces[@]}"; do
    echo "replace ${r}"
  done
} >> "${module_dir}/go.mod"
echo "== in-tree go vet of the published packages =="
(cd "${module_dir}" && "${go_bin}" vet ./pkg/lisafs/ ./pkg/flipcall/ ./pkg/fdchannel/)

echo "PASS: pkg/lisafs, pkg/flipcall, pkg/fdchannel importable under plain go build outside the repo"
