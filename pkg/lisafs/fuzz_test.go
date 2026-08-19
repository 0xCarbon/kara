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

package lisafs

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/lisafs/testsupport"
	"gvisor.dev/gvisor/pkg/unet"
)

// fuzzConnImpl is a minimal ConnectionImpl for dispatch fuzzing (mirrors the
// harness in abi_conformance_test.go / connection_test.go).
type fuzzConnImpl struct{}

type fuzzControlFD struct {
	ControlFD
	ControlFDImpl
}

func (fd *fuzzControlFD) FD() *ControlFD { return &fd.ControlFD }
func (fd *fuzzControlFD) Close()         {}

func (fuzzConnImpl) Mount(c *Connection, mountNode *Node) (*ControlFD, Statx, int, error) {
	root := &fuzzControlFD{}
	mountNode.IncRef()
	root.Init(c, mountNode, linux.ModeDirectory, root)
	return root.FD(), Statx{Mode: uint16(linux.S_IFDIR)}, -1, nil
}
func (fuzzConnImpl) MaxMessageSize() uint32   { return MaxMessageSize() }
func (fuzzConnImpl) SupportedMessages() []MID { return []MID{Mount, Channel} }

// unmarshalTargets are the wire types whose CheckedUnmarshal must never
// panic on attacker-controlled bytes. Each entry constructs a fresh value,
// because dynamic types (e.g. PReadResp) mutate their receiver.
var unmarshalTargets = map[string]func([]byte) bool{
	"ChannelResp": func(b []byte) bool { var v ChannelResp; _, ok := v.CheckedUnmarshal(b); return ok },
	"OpenAtReq":   func(b []byte) bool { var v OpenAtReq; _, ok := v.CheckedUnmarshal(b); return ok },
	"OpenAtResp":  func(b []byte) bool { var v OpenAtResp; _, ok := v.CheckedUnmarshal(b); return ok },
	"PReadReq":    func(b []byte) bool { var v PReadReq; _, ok := v.CheckedUnmarshal(b); return ok },
	"PWriteResp":  func(b []byte) bool { var v PWriteResp; _, ok := v.CheckedUnmarshal(b); return ok },
	"Statx":       func(b []byte) bool { var v Statx; _, ok := v.CheckedUnmarshal(b); return ok },
	"Inode": func(b []byte) bool {
		v := Inode{}
		if len(b) < v.SizeBytes() {
			return false
		}
		v.UnmarshalBytes(b)
		return true
	},
	"MountResp": func(b []byte) bool { var v MountResp; _, ok := v.CheckedUnmarshal(b); return ok },
	"createCommon": func(b []byte) bool {
		v := createCommon{}
		if len(b) < v.SizeBytes() {
			return false
		}
		v.UnmarshalBytes(b)
		return true
	},
	"Getdents64Req":       func(b []byte) bool { var v Getdents64Req; _, ok := v.CheckedUnmarshal(b); return ok },
	"ListenReq":           func(b []byte) bool { var v ListenReq; _, ok := v.CheckedUnmarshal(b); return ok },
	"ConnectReq":          func(b []byte) bool { var v ConnectReq; _, ok := v.CheckedUnmarshal(b); return ok },
	"ConnectWithCredsReq": func(b []byte) bool { var v ConnectWithCredsReq; _, ok := v.CheckedUnmarshal(b); return ok },
	"SetStatReq":          func(b []byte) bool { var v SetStatReq; _, ok := v.CheckedUnmarshal(b); return ok },
	"SetStatResp":         func(b []byte) bool { var v SetStatResp; _, ok := v.CheckedUnmarshal(b); return ok },
	"StatReq":             func(b []byte) bool { var v StatReq; _, ok := v.CheckedUnmarshal(b); return ok },
	"FAllocateReq":        func(b []byte) bool { var v FAllocateReq; _, ok := v.CheckedUnmarshal(b); return ok },
	"CloseReq":            func(b []byte) bool { var v CloseReq; _, ok := v.CheckedUnmarshal(b); return ok },
	"WalkReq":             func(b []byte) bool { var v WalkReq; _, ok := v.CheckedUnmarshal(b); return ok },
	"PWriteReq":           func(b []byte) bool { var v PWriteReq; _, ok := v.CheckedUnmarshal(b); return ok },
	"PReadResp": func(b []byte) bool {
		v := PReadResp{Buf: make([]byte, 1<<16)}
		_, ok := v.CheckedUnmarshal(b)
		return ok
	},
}

// fuzzUnmarshalBody runs every unmarshal target against b. It must not
// panic on any input (a panic is a remote DoS against the server).
func fuzzUnmarshalBody(t testing.TB, b []byte) {
	for name, check := range unmarshalTargets {
		// A panic anywhere in here fails the fuzz target.
		ok := check(b)
		if ok && len(b) == 0 {
			t.Errorf("%s: CheckedUnmarshal accepted empty input", name)
		}
	}
}

// fuzzSeedBytes returns the seed corpus: the committed golden vectors plus
// edge cases (truncations, bogus length prefixes).
func fuzzSeedBytes(t testing.TB) [][]byte {
	var seeds [][]byte
	paths, err := testsupport.GoldenCorpus()
	if err != nil {
		t.Fatalf("golden corpus: %v", err)
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading golden seed: %v", err)
		}
		seeds = append(seeds, b)
	}
	seeds = append(seeds,
		nil,
		[]byte{0},
		[]byte{0xff, 0xff},
		bytes.Repeat([]byte{0xff}, 152),
		append([]byte{0xff, 0xff}, bytes.Repeat([]byte{0x41}, 128)...),
		append([]byte{0x01, 0x00}, bytes.Repeat([]byte{0x00}, 8)...), // 1 FD, zeros
	)
	return seeds
}

// FuzzUnmarshal checks that CheckedUnmarshal of every wire type is total:
// no panics, no acceptance of empty input.
func FuzzUnmarshal(f *testing.F) {
	for _, s := range fuzzSeedBytes(f) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		fuzzUnmarshalBody(t, b)
	})
}

// TestUnmarshalSeedCorpus runs the fuzz seed corpus as a plain unit test so
// it executes under bazel without the fuzzing engine.
func TestUnmarshalSeedCorpus(t *testing.T) {
	for _, s := range fuzzSeedBytes(t) {
		fuzzUnmarshalBody(t, s)
	}
}

// serverDispatchBody drives a real server with a raw framed request built
// from b (first 2 bytes = MID, rest = payload) and requires a well-formed
// framed response: the server must always answer exactly one response per
// request, with a sane MID (echo or Error) and bounded payload.
func serverDispatchBody(t testing.TB, b []byte) {
	if len(b) < 2 {
		return
	}
	mid := MID(binary.LittleEndian.Uint16(b[:2]))
	payload := b[2:]

	serverSock, clientSock, err := unet.SocketPair(false)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	s := NewServer()
	conn, err := s.CreateConnection(serverSock, "/", ConnectionOpts{}, fuzzConnImpl{})
	if err != nil {
		clientSock.Close()
		t.Fatalf("CreateConnection: %v", err)
	}
	s.StartConnection(conn)

	frame := make([]byte, 8+len(payload))
	binary.LittleEndian.PutUint32(frame[0:4], uint32(len(payload)))
	binary.LittleEndian.PutUint16(frame[4:6], uint16(mid))
	if _, err := clientSock.Write(frame); err != nil {
		clientSock.Close()
		s.Destroy()
		t.Fatalf("writing frame: %v", err)
	}

	type resp struct {
		hdr  [8]byte
		n    int
		err  error
		done chan struct{}
	}
	r := resp{done: make(chan struct{})}
	go func() {
		defer close(r.done)
		r.n, r.err = clientSock.Read(r.hdr[:])
	}()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		t.Errorf("MID %d: server did not respond within 10s", mid)
		clientSock.Close()
		s.Destroy()
		return
	}
	if r.err != nil || r.n != 8 {
		t.Errorf("MID %d: response header read n=%d err=%v", mid, r.n, r.err)
	} else {
		payloadLen := binary.LittleEndian.Uint32(r.hdr[0:4])
		respMID := MID(binary.LittleEndian.Uint16(r.hdr[4:6]))
		impl := fuzzConnImpl{}
		if payloadLen > impl.MaxMessageSize() {
			t.Errorf("MID %d: response payloadLen %d exceeds MaxMessageSize", mid, payloadLen)
		}
		if respMID != mid && respMID != Error {
			t.Errorf("MID %d: response MID = %d; want echo or Error(0)", mid, respMID)
		}
		// Drain the announced payload so the server is not blocked.
		if payloadLen > 0 {
			buf := make([]byte, payloadLen)
			clientSock.Read(buf)
		}
	}

	clientSock.Close()
	s.Destroy()
	s.Wait()
}

// FuzzServerDispatch fuzzes the server's request dispatch with hostile
// frames; the contract is ABI.md §Error handling (bounded, well-formed
// responses, never a panic, never silence).
func FuzzServerDispatch(f *testing.F) {
	seeds := [][]byte{
		{byte(Mount), 0},
		{byte(Channel), 0},
		{byte(OpenAt), 0, 42, 0, 0, 0, 0, 0, 0, 0, 0, 0x48, 0x02, 0, 0, 0, 0},
		{0xfa, 0x00}, // unknown MID 250
		{0x00, 0x00}, // Error MID as a request
		{0xff, 0xff, 0xde, 0xad},
		append([]byte{byte(Walk), 0}, make([]byte, 64)...),
		append([]byte{byte(Getdents64), 0}, make([]byte, 16)...),
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		serverDispatchBody(t, b)
	})
}

// TestServerDispatchSeedCorpus runs the dispatch seeds as a unit test.
func TestServerDispatchSeedCorpus(t *testing.T) {
	for _, s := range [][]byte{
		{byte(Mount), 0},
		{byte(Channel), 0},
		{byte(OpenAt), 0, 42, 0, 0, 0, 0, 0, 0, 0, 0, 0x48, 0x02, 0, 0, 0, 0},
		{0xfa, 0x00},
		{0x00, 0x00},
		{0xff, 0xff, 0xde, 0xad},
	} {
		serverDispatchBody(t, s)
	}
}
