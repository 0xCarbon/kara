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

package cmd

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/google/subcommands"

	"gvisor.dev/gvisor/runsc/compat"
	"gvisor.dev/gvisor/runsc/config"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/version"
)

func TestCPUFeaturesCompatKey(t *testing.T) {
	// Capture stdout like TestFeatures does.
	originalStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	os.Stdout = w

	conf := &config.Config{Platform: "kvm"}
	cmd := &CPUFeatures{compatKey: true, compatDriver: "570"}
	status := cmd.Execute(context.Background(), flag.NewFlagSet("cpu-features", flag.ContinueOnError), conf)

	w.Close()
	os.Stdout = originalStdout
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("io.ReadAll failed: %v", err)
	}
	if status != subcommands.ExitSuccess {
		t.Fatalf("Execute returned %v", status)
	}
	got := string(bytes.TrimSpace(out))
	key, err := compat.Parse(got)
	if err != nil {
		t.Fatalf("output %q is not a parseable compat key: %v", got, err)
	}
	if key.Platform != "kvm" || key.Driver != "570" {
		t.Errorf("compat key = %+v, want platform kvm and driver 570", key)
	}
	if key.CPUFeaturesID != compat.CPUFeaturesID() {
		t.Errorf("compat key CPUFeaturesID = %s, want %s (must digest the canonical list)", key.CPUFeaturesID, compat.CPUFeaturesID())
	}
	if key.Build != version.Version() {
		t.Errorf("compat key Build = %q, want %q", key.Build, version.Version())
	}
}

func TestCPUFeaturesListIsCanonical(t *testing.T) {
	originalStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	os.Stdout = w

	cmd := &CPUFeatures{}
	status := cmd.Execute(context.Background(), flag.NewFlagSet("cpu-features", flag.ContinueOnError), &config.Config{})

	w.Close()
	os.Stdout = originalStdout
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("io.ReadAll failed: %v", err)
	}
	if status != subcommands.ExitSuccess {
		t.Fatalf("Execute returned %v", status)
	}
	if got := string(bytes.TrimRight(out, "\n")); got != compat.CanonicalCPUFeatures() {
		t.Errorf("cpu-features output is not the canonical list (drift between CLI and compat package)")
	}
}
