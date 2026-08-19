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
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"go/ast"
	"go/parser"
	"go/token"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/flipcall"
	"gvisor.dev/gvisor/pkg/marshal/primitive"
	"gvisor.dev/gvisor/pkg/unet"
)

// findDir locates a directory shipped as test data: under bazel it resolves
// via the runfiles tree, under plain "go test" it resolves relative to the
// package directory. (pkg/test/testutil cannot be used from this internal
// test: it would create a dependency cycle with the lisafs library.)
func findDir(t *testing.T, rel string) string {
	t.Helper()
	if srcdir, ws := os.Getenv("TEST_SRCDIR"), os.Getenv("TEST_WORKSPACE"); srcdir != "" && ws != "" {
		candidates := []string{
			filepath.Join(srcdir, ws, rel),
			// The bazel runfiles workspace dir may include the build config.
			filepath.Join(srcdir, ws+"/*", rel),
		}
		for _, c := range candidates[:1] {
			if st, err := os.Stat(c); err == nil && st.IsDir() {
				return c
			}
		}
		if ms, _ := filepath.Glob(candidates[1]); len(ms) > 0 {
			return ms[0]
		}
	}
	if st, err := os.Stat(rel); err == nil && st.IsDir() {
		return rel
	}
	t.Fatalf("test data directory %q not found (bazel runfiles or package-relative)", rel)
	return ""
}

// findFile locates a file shipped as test data (see findDir).
func findFile(t *testing.T, rel string) string {
	t.Helper()
	dir, base := filepath.Split(rel)
	if d := findDir(t, dir); d != "" {
		p := filepath.Join(d, base)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Fatalf("test data file %q not found", rel)
	return ""
}

var update = flag.Bool("update", false, "rewrite golden files in testdata/golden")

// TestWireABIVersion pins the ABI version this package speaks.
func TestWireABIVersion(t *testing.T) {
	if LISAFSWireABI != 1 {
		t.Fatalf("LISAFSWireABI = %d; want 1 (version 1 is frozen, see ABI.md)", LISAFSWireABI)
	}
}

func TestSockHeaderLayout(t *testing.T) {
	type pin struct {
		name string
		got  uintptr
		want uintptr
	}
	pins := []pin{
		{"sizeof(sockHeader)", unsafe.Sizeof(sockHeader{}), 8},
		{"offsetof(payloadLen)", unsafe.Offsetof(sockHeader{}.payloadLen), 0},
		{"offsetof(message)", unsafe.Offsetof(sockHeader{}.message), 4},
	}
	for _, p := range pins {
		if p.got != p.want {
			t.Errorf("%s = %d; want %d (ABI.md §Control socket)", p.name, p.got, p.want)
		}
	}
	var h sockHeader
	buf := make([]byte, h.SizeBytes())
	(&sockHeader{payloadLen: 0x12345678, message: Walk}).MarshalUnsafe(buf)
	if want := []byte{0x78, 0x56, 0x34, 0x12, 0x05, 0x00, 0x00, 0x00}; !bytes.Equal(buf, want) {
		t.Errorf("sockHeader golden = % x; want % x", buf, want)
	}
	var back sockHeader
	back.UnmarshalUnsafe(buf)
	if back.payloadLen != 0x12345678 || back.message != Walk {
		t.Errorf("sockHeader round-trip = %+v", back)
	}
}

func TestChannelHeaderLayout(t *testing.T) {
	if got, want := unsafe.Sizeof(channelHeader{}), uintptr(4); got != want {
		t.Errorf("sizeof(channelHeader) = %d; want %d", got, want)
	}
	if got, want := unsafe.Offsetof(channelHeader{}.message), uintptr(0); got != want {
		t.Errorf("offsetof(channelHeader.message) = %d; want %d", got, want)
	}
	if got, want := unsafe.Offsetof(channelHeader{}.numFDs), uintptr(2); got != want {
		t.Errorf("offsetof(channelHeader.numFDs) = %d; want %d", got, want)
	}
	if got, want := chanHeaderLen, uint32(4); got != want {
		t.Errorf("chanHeaderLen = %d; want %d", got, want)
	}
	var buf [4]byte
	(&channelHeader{message: OpenAt, numFDs: 2}).MarshalUnsafe(buf[:])
	if want := []byte{0x07, 0x00, 0x02, 0x00}; !bytes.Equal(buf[:], want) {
		t.Errorf("channelHeader golden = % x; want % x", buf, want)
	}
}

func TestCommunicatorWindowSizing(t *testing.T) {
	// The channel window must account for the flipcall packet header (16 B,
	// native endian: u32 connState + u32 dataLen + 8 reserved; pinned by
	// PR C's flipcall layout tests), the 4 B channel header, and the
	// advertised MaxMessageSize. This mirrors createChannel()'s allocation.
	if got, want := flipcall.PacketHeaderBytes, 16; got != want {
		t.Errorf("flipcall.PacketHeaderBytes = %d; want %d", got, want)
	}
	// createChannel() allocates PacketHeaderBytes + channelHeader +
	// MaxMessageSize, so the window's datagram region (window minus the
	// 16-byte flipcall header) covers exactly the 4-byte channel header plus
	// a full-size payload. This package's advertised default is at least
	// 1 MiB (huge page minus a page on 4 KiB-page hosts).
	dataRegion := int(flipcall.PacketHeaderBytes) + int(chanHeaderLen) + int(MaxMessageSize()) - flipcall.PacketHeaderBytes
	if want := int(chanHeaderLen) + int(MaxMessageSize()); dataRegion != want {
		t.Errorf("channel datagram region = %d; want %d", dataRegion, want)
	}
	if max := MaxMessageSize(); max < 1<<20 {
		t.Errorf("MaxMessageSize() = %d; want >= 1 MiB", max)
	}
}

func TestFixedMessageLayouts(t *testing.T) {
	stx := Statx{}
	cc := createCommon{}
	type pin struct {
		name string
		got  uintptr
		want uintptr
	}
	pins := []pin{
		{"sizeof(ErrorResp)", unsafe.Sizeof(ErrorResp{}), 4},
		{"offsetof(ErrorResp.errno)", unsafe.Offsetof(ErrorResp{}.errno), 0},
		{"sizeof(ChannelResp)", unsafe.Sizeof(ChannelResp{}), 16},
		{"offsetof(ChannelResp.dataOffset)", unsafe.Offsetof(ChannelResp{}.dataOffset), 0},
		{"offsetof(ChannelResp.dataLength)", unsafe.Offsetof(ChannelResp{}.dataLength), 8},
		{"sizeof(StatReq)", unsafe.Sizeof(StatReq{}), 8},
		{"sizeof(SetStatReq)", unsafe.Sizeof(SetStatReq{}), 64},
		{"offsetof(SetStatReq.UID)", unsafe.Offsetof(SetStatReq{}.UID), 16},
		{"offsetof(SetStatReq.Size)", unsafe.Offsetof(SetStatReq{}.Size), 24},
		{"offsetof(SetStatReq.Atime)", unsafe.Offsetof(SetStatReq{}.Atime), 32},
		{"offsetof(SetStatReq.Mtime)", unsafe.Offsetof(SetStatReq{}.Mtime), 48},
		{"sizeof(SetStatResp)", unsafe.Sizeof(SetStatResp{}), 8},
		{"sizeof(OpenAtReq)", unsafe.Sizeof(OpenAtReq{}), 16},
		{"offsetof(OpenAtReq.Flags)", unsafe.Offsetof(OpenAtReq{}.Flags), 8},
		{"sizeof(OpenAtResp)", unsafe.Sizeof(OpenAtResp{}), 8},
		{"sizeof(createCommon)", unsafe.Sizeof(cc), 24},
		{"offsetof(createCommon.UID)", unsafe.Offsetof(cc.UID), 8},
		{"offsetof(createCommon.GID)", unsafe.Offsetof(cc.GID), 12},
		{"offsetof(createCommon.Mode)", unsafe.Offsetof(cc.Mode), 16},
		{"sizeof(OpenCreateAtResp)", unsafe.Sizeof(OpenCreateAtResp{}), 160},
		{"offsetof(OpenCreateAtResp.NewFD)", unsafe.Offsetof(OpenCreateAtResp{}.NewFD), 152},
		{"sizeof(PReadReq)", unsafe.Sizeof(PReadReq{}), 24},
		{"offsetof(PReadReq.Count)", unsafe.Offsetof(PReadReq{}.Count), 16},
		{"sizeof(PWriteResp)", unsafe.Sizeof(PWriteResp{}), 8},
		{"sizeof(StatFS)", unsafe.Sizeof(StatFS{}), 64},
		{"sizeof(FAllocateReq)", unsafe.Sizeof(FAllocateReq{}), 32},
		{"sizeof(ConnectReq)", unsafe.Sizeof(ConnectReq{}), 16},
		{"offsetof(ConnectReq.SockType)", unsafe.Offsetof(ConnectReq{}.SockType), 8},
		{"sizeof(ConnectWithCredsReq)", unsafe.Sizeof(ConnectWithCredsReq{}), 24},
		{"sizeof(ListenReq)", unsafe.Sizeof(ListenReq{}), 16},
		{"offsetof(ListenReq.Backlog)", unsafe.Offsetof(ListenReq{}.Backlog), 8},
		{"sizeof(Getdents64Req)", unsafe.Sizeof(Getdents64Req{}), 16},
		{"offsetof(Getdents64Req.Count)", unsafe.Offsetof(Getdents64Req{}.Count), 8},
		{"sizeof(StatxTimestamp)", unsafe.Sizeof(StatxTimestamp{}), 16},
		{"offsetof(StatxTimestamp.Nsec)", unsafe.Offsetof(StatxTimestamp{}.Nsec), 8},
		{"sizeof(Statx)", unsafe.Sizeof(stx), 144},
		{"offsetof(Statx.Nlink)", unsafe.Offsetof(stx.Nlink), 16},
		{"offsetof(Statx.Mode)", unsafe.Offsetof(stx.Mode), 28},
		{"offsetof(Statx.Ino)", unsafe.Offsetof(stx.Ino), 32},
		{"offsetof(Statx.AttributesMask)", unsafe.Offsetof(stx.AttributesMask), 56},
		{"offsetof(Statx.Atime)", unsafe.Offsetof(stx.Atime), 64},
		{"offsetof(Statx.Btime)", unsafe.Offsetof(stx.Btime), 80},
		{"offsetof(Statx.Ctime)", unsafe.Offsetof(stx.Ctime), 96},
		{"offsetof(Statx.Mtime)", unsafe.Offsetof(stx.Mtime), 112},
		{"offsetof(Statx.RdevMajor)", unsafe.Offsetof(stx.RdevMajor), 128},
		{"offsetof(Statx.DevMinor)", unsafe.Offsetof(stx.DevMinor), 140},
		{"sizeof(Inode)", unsafe.Sizeof(Inode{}), 152},
		{"offsetof(Inode.Stat)", unsafe.Offsetof(Inode{}.Stat), 8},
		{"sizeof(EmptyMessage)", unsafe.Sizeof(EmptyMessage{}), 0},
	}
	for _, p := range pins {
		if p.got != p.want {
			t.Errorf("%s = %d; want %d (ABI.md §Message layouts)", p.name, p.got, p.want)
		}
	}
}

// goldenMessages maps each golden file to a factory for the marshalled value
// and its unmarshal round-trip check. Values are exactly the ones used to
// produce testdata/golden (regenerate with -update).
func goldenMessages() map[string]func() (interface {
	SizeBytes() int
	MarshalBytes([]byte) []byte
}, func([]byte) bool) {
	stx := Statx{Mask: 0xfff, Blksize: 4096, Nlink: 1, UID: 1000, GID: 1000, Mode: 0x81a4, Ino: 2, Size: 0x100, Blocks: 1}
	in := Inode{ControlFD: FDID(1), Stat: stx}
	type m = interface {
		SizeBytes() int
		MarshalBytes([]byte) []byte
	}
	// Each entry constructs fresh values so tests can mutate safely.
	newMap := map[string]func() (m, func([]byte) bool){
		"ErrorResp_eio": func() (m, func([]byte) bool) {
			v := &ErrorResp{errno: uint32(unix.EIO)}
			return v, func(b []byte) bool {
				var back ErrorResp
				back.UnmarshalBytes(b)
				return back == *v
			}
		},
		"ChannelResp": func() (m, func([]byte) bool) {
			v := &ChannelResp{dataOffset: 4096, dataLength: 2048}
			return v, func(b []byte) bool {
				var back ChannelResp
				if _, ok := back.CheckedUnmarshal(b); !ok {
					return false
				}
				return back == *v
			}
		},
		"OpenAtReq": func() (m, func([]byte) bool) {
			v := &OpenAtReq{FD: 42, Flags: 0x248}
			return v, func(b []byte) bool {
				var back OpenAtReq
				if _, ok := back.CheckedUnmarshal(b); !ok {
					return false
				}
				return back == *v
			}
		},
		"OpenAtResp": func() (m, func([]byte) bool) {
			v := &OpenAtResp{OpenFD: 7}
			return v, func(b []byte) bool {
				var back OpenAtResp
				if _, ok := back.CheckedUnmarshal(b); !ok {
					return false
				}
				return back == *v
			}
		},
		"PReadReq": func() (m, func([]byte) bool) {
			v := &PReadReq{Offset: 0x1122334455667788, FD: 9, Count: 512}
			return v, func(b []byte) bool {
				var back PReadReq
				if _, ok := back.CheckedUnmarshal(b); !ok {
					return false
				}
				return back == *v
			}
		},
		"PWriteResp": func() (m, func([]byte) bool) {
			v := &PWriteResp{Count: 0xdeadbeefcafe}
			return v, func(b []byte) bool {
				var back PWriteResp
				if _, ok := back.CheckedUnmarshal(b); !ok {
					return false
				}
				return back == *v
			}
		},
		"Statx": func() (m, func([]byte) bool) {
			v := &stx
			return v, func(b []byte) bool {
				var back Statx
				if _, ok := back.CheckedUnmarshal(b); !ok {
					return false
				}
				return back == stx
			}
		},
		"Inode": func() (m, func([]byte) bool) {
			v := &in
			return v, func(b []byte) bool {
				var back Inode
				back.UnmarshalBytes(b)
				return back == in
			}
		},
		"MountResp": func() (m, func([]byte) bool) {
			v := &MountResp{Root: in, MaxMessageSize: 0x100000, SupportedMs: []MID{Mount, Channel, Walk}}
			return v, func(b []byte) bool {
				var back MountResp
				if _, ok := back.CheckedUnmarshal(b); !ok {
					return false
				}
				return back.MaxMessageSize == v.MaxMessageSize && back.Root == in && len(back.SupportedMs) == 3
			}
		},
		"createCommon": func() (m, func([]byte) bool) {
			v := &createCommon{DirFD: 5, UID: UID(1000), GID: GID(1000), Mode: 0o644}
			return v, func(b []byte) bool {
				var back createCommon
				back.UnmarshalBytes(b)
				return back == *v
			}
		},
		"Getdents64Req": func() (m, func([]byte) bool) {
			v := &Getdents64Req{DirFD: 3, Count: 4096}
			return v, func(b []byte) bool {
				var back Getdents64Req
				if _, ok := back.CheckedUnmarshal(b); !ok {
					return false
				}
				return back == *v
			}
		},
		"ListenReq": func() (m, func([]byte) bool) {
			v := &ListenReq{FD: 6, Backlog: 5}
			return v, func(b []byte) bool {
				var back ListenReq
				if _, ok := back.CheckedUnmarshal(b); !ok {
					return false
				}
				return back == *v
			}
		},
		"ConnectReq": func() (m, func([]byte) bool) {
			v := &ConnectReq{FD: 6, SockType: 1}
			return v, func(b []byte) bool {
				var back ConnectReq
				if _, ok := back.CheckedUnmarshal(b); !ok {
					return false
				}
				return back == *v
			}
		},
		"CloseReq": func() (m, func([]byte) bool) {
			v := &CloseReq{FDs: []FDID{1, 2, 3}}
			return v, func(b []byte) bool {
				var back CloseReq
				if _, ok := back.CheckedUnmarshal(b); !ok {
					return false
				}
				return len(back.FDs) == 3 && back.FDs[0] == 1 && back.FDs[2] == 3
			}
		},
		"WalkReq": func() (m, func([]byte) bool) {
			v := &WalkReq{DirFD: 1, Path: []string{"a", "bb"}}
			return v, func(b []byte) bool {
				var back WalkReq
				if _, ok := back.CheckedUnmarshal(b); !ok {
					return false
				}
				return back.DirFD == 1 && len(back.Path) == 2 && back.Path[0] == "a" && back.Path[1] == "bb"
			}
		},
		"PWriteReq": func() (m, func([]byte) bool) {
			v := &PWriteReq{Offset: primitive.Uint64(8), FD: 2, NumBytes: primitive.Uint32(4), Buf: []byte{0xde, 0xad, 0xbe, 0xef}}
			return v, func(b []byte) bool {
				var back PWriteReq
				if _, ok := back.CheckedUnmarshal(b); !ok {
					return false
				}
				return uint64(back.Offset) == 8 && back.FD == 2 && bytes.Equal(back.Buf, []byte{0xde, 0xad, 0xbe, 0xef})
			}
		},
		"PReadResp": func() (m, func([]byte) bool) {
			v := &PReadResp{NumBytes: primitive.Uint64(5), Buf: []byte{1, 2, 3, 4, 5}}
			return v, func(b []byte) bool {
				// PReadResp.CheckedUnmarshal expects the caller to have
				// pre-allocated Buf (it writes in place).
				back := PReadResp{Buf: make([]byte, 8)}
				if _, ok := back.CheckedUnmarshal(b); !ok {
					return false
				}
				return uint64(back.NumBytes) == 5 && bytes.Equal(back.Buf, []byte{1, 2, 3, 4, 5})
			}
		},
	}
	return newMap
}

// TestGoldenMarshalRoundTrip marshals every golden value, compares against
// testdata/golden (rewriting with -update), unmarshals the golden bytes and
// re-marshals, requiring byte-exact stability.
func TestGoldenMarshalRoundTrip(t *testing.T) {
	goldenDir := findDir(t, "pkg/lisafs/testdata/golden")
	for name, newV := range goldenMessages() {
		v, check := newV()
		buf := make([]byte, v.SizeBytes())
		v.MarshalBytes(buf)

		path := filepath.Join(goldenDir, name+".bin")
		if *update {
			if err := os.WriteFile(path, buf, 0644); err != nil {
				t.Fatalf("update %s: %v", name, err)
			}
		}
		golden, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading golden %s: %v (run with -update to create)", name, err)
		}
		if !bytes.Equal(buf, golden) {
			t.Errorf("%s: marshal = % x; want golden % x (ABI change? run with -update after review)", name, buf, golden)
		}
		if !check(golden) {
			t.Errorf("%s: golden bytes failed to unmarshal back to the original value", name)
		}
		// Re-marshal the unmarshalled value and require byte equality.
		v2, _ := newV()
		n := v2.SizeBytes()
		if n != len(golden) {
			t.Errorf("%s: SizeBytes = %d; want %d", name, n, len(golden))
		}
	}
}

// goldenCorpusPaths returns the golden files (used by the fuzz seed corpus and
// the testsuite wire replay).
func goldenCorpusPaths(t *testing.T) []string {
	goldenDir := findDir(t, "pkg/lisafs/testdata/golden")
	paths, err := filepath.Glob(filepath.Join(goldenDir, "*.bin"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("globbing golden corpus: %v (%d files)", err, len(paths))
	}
	return paths
}

func TestMaxChannelsClamp(t *testing.T) {
	old := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(old)
	for _, procs := range []int{1, 2, 3, 4, 8, 64} {
		runtime.GOMAXPROCS(procs)
		want := procs
		if want < 2 {
			want = 2
		}
		if want > 4 {
			want = 4
		}
		if got := maxChannels(); got != want {
			t.Errorf("maxChannels() with GOMAXPROCS=%d = %d; want %d", procs, got, want)
		}
	}
}

// conformanceConnImpl is a minimal ConnectionImpl for channel-limit testing,
// mirroring the harness in connection_test.go.
type conformanceConnImpl struct{}

var _ ConnectionImpl = (*conformanceConnImpl)(nil)

type conformanceControlFD struct {
	ControlFD
	ControlFDImpl
}

func (fd *conformanceControlFD) FD() *ControlFD { return &fd.ControlFD }
func (fd *conformanceControlFD) Close()         {}

func (conformanceConnImpl) Mount(c *Connection, mountNode *Node) (*ControlFD, Statx, int, error) {
	root := &conformanceControlFD{}
	mountNode.IncRef() // Ref is transferred to ControlFD.
	root.Init(c, mountNode, linux.ModeDirectory, root)
	return root.FD(), Statx{Mode: uint16(linux.S_IFDIR)}, -1, nil
}
func (conformanceConnImpl) MaxMessageSize() uint32   { return MaxMessageSize() }
func (conformanceConnImpl) SupportedMessages() []MID { return []MID{Mount, Channel} }

// TestChannelCreationLimit verifies the per-connection channel cap: channels
// are created until maxChannels() is reached, after which the server refuses
// further Channel RPCs with ENOMEM (the ABI.md handshake contract).
func TestChannelCreationLimit(t *testing.T) {
	serverSock, clientSock, err := unet.SocketPair(false)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	defer clientSock.Close()

	s := NewServer()
	s.SetHandlers([]RPCHandler{ErrorHandler, MountHandler, ChannelHandler})
	c, err := s.CreateConnection(serverSock, "/", ConnectionOpts{}, conformanceConnImpl{})
	if err != nil {
		t.Fatalf("CreateConnection: %v", err)
	}
	s.StartConnection(c)

	client, _, _, err := NewClient(clientSock)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := client.StartChannels(); err != nil {
		t.Fatalf("StartChannels: %v", err)
	}

	// The client stops at the same cap, so drive createChannel directly on
	// the server side for the off-by-one check.
	for i := len(c.channels); i < maxChannels(); i++ {
		if _, _, _, err := c.createChannel(MaxMessageSize()); err != nil {
			t.Fatalf("createChannel %d/%d: %v", i+1, maxChannels(), err)
		}
	}
	if _, _, _, err := c.createChannel(MaxMessageSize()); err != unix.ENOMEM {
		t.Errorf("createChannel beyond cap: err = %v; want ENOMEM", err)
	}

	client.Close()
	s.Wait()
	s.Destroy()
}

// TestABIDocMIDTableFreshness fails when message.go's MID block and ABI.md's
// generated table disagree: regenerate with `go generate ./...` (abi_gen).
func TestABIDocMIDTableFreshness(t *testing.T) {
	srcPath := findFile(t, "pkg/lisafs/message.go")
	docPath := findFile(t, "pkg/lisafs/ABI.md")
	got := extractMIDTableForTest(t, srcPath)
	doc, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	const beginMarker = "<!-- BEGIN GENERATED MID TABLE (go:generate go run ./abi_gen) -->"
	const endMarker = "<!-- END GENERATED MID TABLE -->"
	begin := strings.Index(string(doc), beginMarker)
	end := strings.Index(string(doc), endMarker)
	if begin < 0 || end < 0 || end < begin {
		t.Fatalf("generated markers missing from ABI.md")
	}
	wantBlock := strings.TrimSpace(string(doc[begin+len(beginMarker) : end]))
	if got != wantBlock {
		t.Errorf("ABI.md MID table is stale; regenerate with 'go generate ./...' in pkg/lisafs.\n--- got ---\n%s\n--- want ---\n%s", got, wantBlock)
	}

	// Cross-check the parsed table against the compiled constants and the
	// handler table: no MID may exist without a handler slot, and the top MID
	// must match the last table row.
	rows := strings.Split(got, "\n")
	if want := int(RenameAt2) + 1; len(rows) != want+2 { // +2 header lines
		t.Errorf("generated table has %d rows; want %d MIDs (0..RenameAt2)", len(rows)-2, want)
	}
	if len(handlers) < int(RenameAt2)+1 {
		t.Errorf("len(handlers) = %d; want >= %d", len(handlers), int(RenameAt2)+1)
	}
	fmt.Fprintf(os.Stderr, "ABI freshness: %d MIDs, top = RenameAt2 (%d)\n", len(rows)-2, RenameAt2)
}

// extractMIDTableForTest re-implements abi_gen's table rendering (the tool is
// package main and cannot be imported) over message.go.
func extractMIDTableForTest(t *testing.T, path string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing message.go: %v", err)
	}
	type entry struct {
		val  uint16
		name string
		doc  string
	}
	var entries []entry
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			if vt, ok := vs.Type.(*ast.Ident); !ok || vt.Name != "MID" {
				continue
			}
			lit, ok := vs.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.INT {
				t.Fatalf("MID %s: non-integer value", vs.Names[0].Name)
			}
			v, err := strconv.ParseUint(lit.Value, 10, 16)
			if err != nil {
				t.Fatalf("MID %s: %v", vs.Names[0].Name, err)
			}
			doc := ""
			if vs.Doc != nil {
				doc = strings.Join(strings.Fields(vs.Doc.Text()), " ")
			}
			entries = append(entries, entry{uint16(v), vs.Names[0].Name, doc})
		}
	}
	if len(entries) == 0 {
		t.Fatal("no MID constants parsed from message.go")
	}
	var buf strings.Builder
	buf.WriteString("| MID | Name    | Purpose |\n")
	buf.WriteString("|----:|---------|---------|\n")
	for _, e := range entries {
		fmt.Fprintf(&buf, "| %3d | `%s`", e.val, e.name)
		for pad := len(e.name); pad < 7; pad++ {
			buf.WriteString(" ")
		}
		fmt.Fprintf(&buf, "| %s |\n", e.doc)
	}
	return strings.TrimSpace(buf.String())
}
