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

package compat

import (
	"strings"
	"testing"
)

func TestKeyRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		k    Key
	}{
		{"cpu sandbox", Key{Build: "release-1", Platform: "systrap", CPUFeaturesID: "0123456789abcdef"}},
		{"gpu sandbox", Key{Build: "release-1", Platform: "kvm", CPUFeaturesID: "0123456789abcdef", Driver: "570"}},
		{"empty trailing", Key{Build: "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.k.String()
			got, err := Parse(s)
			if err != nil {
				t.Fatalf("Parse(%q): %v", s, err)
			}
			if got != tc.k {
				t.Errorf("round trip mismatch: got %v, want %v (serialized %q)", got, tc.k, s)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"", "malformed"},
		{"v1|a|b", "malformed"},
		{"v1|a|b|c|d|e", "malformed"},
		{"v2|a|b|c|d", "unsupported"},
	} {
		if _, err := Parse(tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Parse(%q) err = %v, want containing %q", tc.in, err, tc.want)
		}
	}
}

func TestValidateRejectsSeparator(t *testing.T) {
	k := Key{Build: "a|b"}
	if err := k.Validate(); err == nil {
		t.Errorf("Validate must reject '|' inside fields")
	}
	if _, err := Parse(k.String()); err == nil {
		t.Errorf("Parse must reject keys with '|' inside fields")
	}
}

func TestCompatible(t *testing.T) {
	base := Key{Build: "b", Platform: "p", CPUFeaturesID: "c", Driver: ""}
	for name, mutate := range map[string]func(Key) Key{
		"same":        func(k Key) Key { return k },
		"build":       func(k Key) Key { k.Build = "other"; return k },
		"platform":    func(k Key) Key { k.Platform = "other"; return k },
		"cpuid":       func(k Key) Key { k.CPUFeaturesID = "other"; return k },
		"driver":      func(k Key) Key { k.Driver = "570"; return k },
		"cpuid-empty": func(k Key) Key { k.CPUFeaturesID = ""; return k },
	} {
		image := mutate(base)
		want := name == "same"
		if got := base.Compatible(image); got != want {
			t.Errorf("Compatible(%s) = %v, want %v", name, got, want)
		}
	}
}

func TestSerializedBudget(t *testing.T) {
	// Placement metadata budgets (e.g. oca's memberlist-meta cap) must fit
	// the serialized key comfortably; pin a generous hard ceiling.
	k := Key{Build: strings.Repeat("b", 40), Platform: strings.Repeat("p", 16), CPUFeaturesID: strings.Repeat("c", 16), Driver: strings.Repeat("d", 16)}
	if n := len(k.String()); n > 128 {
		t.Errorf("serialized key too long even with maximal fields: %d bytes", n)
	}
}

func TestCanonicalCPUFeaturesStable(t *testing.T) {
	a := CanonicalCPUFeatures()
	b := CanonicalCPUFeatures()
	if a != b {
		t.Errorf("CanonicalCPUFeatures not deterministic: %q vs %q", a, b)
	}
	if strings.ContainsRune(a, '|') {
		t.Errorf("canonical feature list must not contain '|': %q", a)
	}
	if id := CPUFeaturesID(); len(id) != 16 {
		t.Errorf("CPUFeaturesID length = %d, want 16 hex chars", len(id))
	} else if CPUFeaturesID() != id {
		t.Errorf("CPUFeaturesID not deterministic")
	}
}

func TestHostKeyShape(t *testing.T) {
	k := HostKey("systrap", "")
	if k.Platform != "systrap" || k.Driver != "" || k.CPUFeaturesID == "" {
		t.Errorf("HostKey missing components: %+v", k)
	}
	gpu := HostKey("kvm", "570")
	if !k.Compatible(k) || k.Compatible(gpu) {
		t.Errorf("compatibility rules broken for %+v vs %+v", k, gpu)
	}
	if _, err := Parse(k.String()); err != nil {
		t.Errorf("HostKey serialization not parseable: %v", err)
	}
}
