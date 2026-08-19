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

package testsuite

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gvisor.dev/gvisor/pkg/lisafs"
	"gvisor.dev/gvisor/pkg/lisafs/testsupport"
	"gvisor.dev/gvisor/pkg/unet"
)

// RunWireConformanceTest replays the committed golden request corpus
// (pkg/lisafs/testdata/golden, produced by the real go_marshal encoders) as
// raw control-socket frames against a live server built from tester, using
// an independent hand-rolled codec (mirroring a third-party implementer like
// Oca's gofer). The wire contract being checked is pkg/lisafs/ABI.md: exact
// 8-byte sockHeader framing, response MID echo or Error(0), bounded
// payloads, and no panics or silence on any input.
func RunWireConformanceTest(t *testing.T, tester Tester) {
	serverSocket, clientSocket, err := unet.SocketPair(false)
	if err != nil {
		t.Fatalf("socketpair got err %v expected nil", err)
	}

	server := lisafs.NewServer()
	impl := tester.NewConnImpl(t)
	conn, err := server.CreateConnection(serverSocket, t.TempDir(), lisafs.ConnectionOpts{}, impl)
	if err != nil {
		t.Fatalf("starting connection failed: %v", err)
	}
	server.StartConnection(conn)
	defer func() {
		clientSocket.Close()
		server.Destroy()
		server.Wait()
	}()

	// Handshake first (ABI.md §Connection lifecycle): a Mount frame with an
	// empty payload, then one Channel frame. The responses' framing is
	// asserted; their payloads are implementation-specific.
	for _, m := range []lisafs.MID{lisafs.Mount, lisafs.Channel} {
		respMID, payloadLen := sndRawFrame(t, clientSocket, m, nil)
		if respMID != m {
			t.Fatalf("handshake %d: response MID = %d; want echo", m, respMID)
		}
		if m == lisafs.Mount && payloadLen == 0 {
			t.Fatal("Mount response has no payload")
		}
	}

	// Replay the golden request corpus as raw frames.
	golden, err := testsupport.GoldenCorpus()
	if err != nil {
		t.Fatalf("golden corpus: %v", err)
	}
	for _, path := range golden {
		name := strings.TrimSuffix(filepath.Base(path), ".bin")
		m, ok := goldenRequestMIDs[name]
		if !ok {
			continue // headers/responses: exercised in abi_conformance_test.
		}
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading golden %s: %v", name, err)
		}
		t.Run(name, func(t *testing.T) {
			respMID, payloadLen := sndRawFrame(t, clientSocket, m, payload)
			if respMID != m && respMID != lisafs.Error {
				t.Errorf("response MID = %d; want echo of %d or Error(0)", respMID, m)
			}
			if payloadLen > impl.MaxMessageSize() {
				t.Errorf("response payloadLen = %d; exceeds MaxMessageSize %d", payloadLen, impl.MaxMessageSize())
			}
		})
	}
}

// goldenRequestMIDs maps each replayable golden file to its MID.
var goldenRequestMIDs = map[string]lisafs.MID{
	"CloseReq":      lisafs.Close,
	"ConnectReq":    lisafs.Connect,
	"Getdents64Req": lisafs.Getdents64,
	"ListenReq":     lisafs.Listen,
	"OpenAtReq":     lisafs.OpenAt,
	"PReadReq":      lisafs.PRead,
	"PWriteReq":     lisafs.PWrite,
	"WalkReq":       lisafs.Walk,
}

// sndRawFrame writes one control-socket frame (8-byte sockHeader + payload,
// little-endian, independent of pkg/lisafs' marshal code) and reads one
// framed response, returning its MID and payload length.
func sndRawFrame(t *testing.T, sock *unet.Socket, m lisafs.MID, payload []byte) (lisafs.MID, uint32) {
	t.Helper()
	frame := make([]byte, 8+len(payload))
	binary.LittleEndian.PutUint32(frame[0:4], uint32(len(payload)))
	binary.LittleEndian.PutUint16(frame[4:6], uint16(m))
	copy(frame[8:], payload)
	if _, err := sock.Write(frame); err != nil {
		t.Fatalf("writing MID %d frame: %v", m, err)
	}

	var hdr [8]byte
	if n, err := sock.Read(hdr[:]); err != nil || n != 8 {
		t.Fatalf("reading response header: n=%d err=%v", n, err)
	}
	respMID := lisafs.MID(binary.LittleEndian.Uint16(hdr[4:6]))
	payloadLen := binary.LittleEndian.Uint32(hdr[0:4])
	if payloadLen > 0 {
		buf := make([]byte, payloadLen)
		if n, err := sock.Read(buf); err != nil || uint32(n) != payloadLen {
			t.Fatalf("reading response payload: n=%d want=%d err=%v", n, payloadLen, err)
		}
	}
	return respMID, payloadLen
}
