// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package testsupport provides shared helpers for pkg/lisafs tests (which
// live in multiple bazel go_test binaries and cannot share _test.go files).
package testsupport

import (
	"fmt"
	"os"
	"path/filepath"
)

// FindDir locates a directory shipped as test data: under bazel it resolves
// via the runfiles tree, under plain "go test" it resolves relative to the
// package directory. (pkg/test/testutil cannot be used from internal lisafs
// tests: it would create a dependency cycle with the lisafs library.)
func FindDir(rel string) (string, error) {
	if srcdir, ws := os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"); srcdir != "" && ws != "" {
		direct := filepath.Join(srcdir, ws, rel)
		if st, err := os.Stat(direct); err == nil && st.IsDir() {
			return direct, nil
		}
		// The bazel runfiles workspace dir may include the build config.
		if ms, _ := filepath.Glob(filepath.Join(srcdir, ws+"/*", rel)); len(ms) > 0 {
			return ms[0], nil
		}
	}
	if st, err := os.Stat(rel); err == nil && st.IsDir() {
		return rel, nil
	}
	return "", fmt.Errorf("test data directory %q not found (bazel runfiles or package-relative)", rel)
}

// GoldenCorpus returns the paths of the committed ABI golden vectors.
func GoldenCorpus() ([]string, error) {
	dir, err := FindDir("pkg/lisafs/testdata/golden")
	if err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.bin"))
	if err != nil || len(paths) == 0 {
		return nil, fmt.Errorf("golden corpus not found in %s: %v", dir, err)
	}
	return paths, nil
}
