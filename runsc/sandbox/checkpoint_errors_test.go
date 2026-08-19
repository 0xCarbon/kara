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

package sandbox

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip"
)

// urpcFlatten mimics how the control channel flattens sandbox-side errors:
// only the Error() string survives the RPC boundary.
func urpcFlatten(method string, err error) error {
	return fmt.Errorf("urpc method %q failed: %s", method, err.Error())
}

// TestSaveRejectionPrefixMatchesTCPip pins the classification prefix to the
// real tcpip.ErrSaveRejection wire format so the two cannot drift.
func TestSaveRejectionPrefixMatchesTCPip(t *testing.T) {
	sent := (&tcpip.ErrSaveRejection{Err: errors.New("boom")}).Error()
	if !strings.HasPrefix(sent, saveRejectionPrefix+":") {
		t.Errorf("tcpip.ErrSaveRejection text %q no longer starts with the classified prefix %q", sent, saveRejectionPrefix)
	}
}

func TestClassifyCheckpointError(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   error
		// want* classify the expected taxonomy result; text is the expected
		// Error() of the classified error.
		wantReject  bool
		wantDeath   bool
		wantContain string
	}{
		{
			name:        "save rejection (raw)",
			in:          &tcpip.ErrSaveRejection{Err: errors.New("endpoint still has waiters")},
			wantReject:  true,
			wantContain: "endpoint still has waiters",
		},
		{
			name:        "save rejection (urpc flattened)",
			in:          urpcFlatten("containerManager.Checkpoint", &tcpip.ErrSaveRejection{Err: errors.New("endpoint still has waiters")}),
			wantReject:  true,
			wantContain: "endpoint still has waiters",
		},
		{
			name:        "sandbox death (urpc EOF)",
			in:          urpcFlatten("containerManager.Checkpoint", io.EOF),
			wantDeath:   true,
			wantContain: "EOF",
		},
		{
			name:        "sandbox death (urpc connection reset)",
			in:          urpcFlatten("containerManager.Checkpoint", syscall.ECONNRESET),
			wantDeath:   true,
			wantContain: "connection reset by peer",
		},
		{
			name:        "sandbox death (wrapped io.EOF)",
			in:          fmt.Errorf("wrote request: %w", io.EOF),
			wantDeath:   true,
			wantContain: "EOF",
		},
		{
			name:        "unclassified passthrough",
			in:          errors.New("checkpointing container \"c\": some other failure"),
			wantContain: "some other failure",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyCheckpointError(tc.in)
			if got == nil {
				t.Fatalf("classifyCheckpointError(nil-result) for %v", tc.in)
			}
			if gotReject := IsSaveRejection(got); gotReject != tc.wantReject {
				t.Errorf("IsSaveRejection = %v, want %v (err: %v)", gotReject, tc.wantReject, got)
			}
			if gotDeath := IsSandboxDeath(got); gotDeath != tc.wantDeath {
				t.Errorf("IsSandboxDeath = %v, want %v (err: %v)", gotDeath, tc.wantDeath, got)
			}
			if tc.wantReject && tc.wantDeath {
				t.Errorf("classification is both rejection and death: %v", got)
			}
			if !strings.Contains(got.Error(), tc.wantContain) {
				t.Errorf("classified error %q does not contain %q", got.Error(), tc.wantContain)
			}
			// The original error must stay reachable through the wrap chain
			// except for rejections, which are reconstructed from text
			// (only the string crosses the RPC boundary).
			if !tc.wantReject && !errors.Is(got, tc.in) && tc.in != io.EOF {
				t.Errorf("errors.Is(classified, original) = false for %v", got)
			}
		})
	}
}

// TestClassifyNil ensures the helper is nil-safe.
func TestClassifyNil(t *testing.T) {
	if got := classifyCheckpointError(nil); got != nil {
		t.Errorf("classifyCheckpointError(nil) = %v, want nil", got)
	}
	if IsSaveRejection(nil) || IsSandboxDeath(nil) {
		t.Errorf("Is*(nil) must be false")
	}
}

// TestSaveRejectionErrorShape pins the typed error's wire text: embedders
// that log or persist the error string rely on it starting with the stable
// rejection prefix.
func TestSaveRejectionErrorShape(t *testing.T) {
	e := &SaveRejection{Detail: "detail text"}
	want := saveRejectionPrefix + ": detail text"
	if got := e.Error(); got != want {
		t.Errorf("SaveRejection.Error() = %q, want %q", got, want)
	}
	var target *SaveRejection
	if !errors.As(fmt.Errorf("outer: %w", e), &target) {
		t.Errorf("errors.As through a wrap chain failed")
	}
}
