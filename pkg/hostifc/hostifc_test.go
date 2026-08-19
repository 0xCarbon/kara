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

package hostifc

import (
	"bytes"
	"errors"
	"testing"

	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/fdchannel"
	"gvisor.dev/gvisor/pkg/flipcall"
)

// TestDefaultIPCControlSocketDonation exercises the control-socket seam with
// the Linux backend: a stream pair carrying an in-band FD donation, the same
// shape pkg/lisafs uses for Mount and Channel setup RPCs.
func TestDefaultIPCControlSocketDonation(t *testing.T) {
	ipc := Default()
	a, b, err := ipc.NewControlSocketPair()
	if err != nil {
		t.Fatalf("NewControlSocketPair(): %v", err)
	}
	defer a.Close()
	defer b.Close()

	// Donate the write end of a pipe along with a payload byte.
	var pipeFDs [2]int
	if err := unix.Pipe2(pipeFDs[:], 0); err != nil {
		t.Fatalf("unix.Pipe2(): %v", err)
	}
	r, w := pipeFDs[0], pipeFDs[1]
	defer unix.Close(r)
	defer unix.Close(w)

	payload := []byte{0x5a}
	wr := a.Writer(true)
	wr.PackFDs(w)
	if _, err := wr.WriteVec([][]byte{payload}); err != nil {
		t.Fatalf("WriteVec(): %v", err)
	}

	rd := b.Reader(true)
	rd.EnableFDs(1)
	got := make([]byte, 1)
	if _, err := rd.ReadVec([][]byte{got}); err != nil {
		t.Fatalf("ReadVec(): %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("ReadVec() got %v, wanted %v", got, payload)
	}
	fds, err := rd.ExtractFDs()
	if err != nil {
		t.Fatalf("ExtractFDs(): %v", err)
	}
	if len(fds) != 1 {
		t.Fatalf("ExtractFDs() returned %d FDs, wanted 1", len(fds))
	}
	defer unix.Close(fds[0])

	// The donated descriptor must be the pipe write end: write through it
	// and observe the byte on the original read end.
	if _, err := unix.Write(fds[0], payload); err != nil {
		t.Fatalf("write to donated FD: %v", err)
	}
	pipeGot := make([]byte, 1)
	if _, err := unix.Read(r, pipeGot); err != nil {
		t.Fatalf("read from pipe: %v", err)
	}
	if !bytes.Equal(pipeGot, payload) {
		t.Fatalf("pipe read got %v, wanted %v", pipeGot, payload)
	}
}

// TestDefaultIPCFDChannel exercises the FD-channel seam with the Linux
// backend: a connected donation pair in the flipcall/lisafs channel shape
// (one FD per message, in order).
func TestDefaultIPCFDChannel(t *testing.T) {
	ipc := Default()
	local, peerFD, err := ipc.NewFDChannel()
	if err != nil {
		t.Fatalf("NewFDChannel(): %v", err)
	}
	defer local.Destroy()
	peer := fdchannel.NewEndpoint(peerFD)
	defer peer.Destroy()

	var pipeFDs [2]int
	if err := unix.Pipe2(pipeFDs[:], 0); err != nil {
		t.Fatalf("unix.Pipe2(): %v", err)
	}
	r, w := pipeFDs[0], pipeFDs[1]
	defer unix.Close(r)
	defer unix.Close(w)

	if err := local.SendFD(w); err != nil {
		t.Fatalf("SendFD(): %v", err)
	}
	got, err := peer.RecvFD()
	if err != nil {
		t.Fatalf("RecvFD(): %v", err)
	}
	defer unix.Close(got)
	if _, err := unix.Write(got, []byte{1}); err != nil {
		t.Fatalf("write to donated FD: %v", err)
	}
	one := make([]byte, 1)
	if _, err := unix.Read(r, one); err != nil {
		t.Fatalf("read from pipe: %v", err)
	}
}

// TestDefaultIPCWindowAllocator exercises the packet-window seam with the
// Linux backend: page-aligned, disjoint windows from one donatable
// descriptor.
func TestDefaultIPCWindowAllocator(t *testing.T) {
	ipc := Default()
	alloc, err := ipc.NewPacketWindowAllocator()
	if err != nil {
		t.Fatalf("NewPacketWindowAllocator(): %v", err)
	}
	defer alloc.Destroy()

	d1, err := alloc.Allocate(4096)
	if err != nil {
		t.Fatalf("Allocate(4096): %v", err)
	}
	d2, err := alloc.Allocate(4096)
	if err != nil {
		t.Fatalf("Allocate(4096): %v", err)
	}
	if d1.FD != d2.FD {
		t.Fatalf("windows from different descriptors: %d vs %d", d1.FD, d2.FD)
	}
	pageSize := int64(unix.Getpagesize())
	if d1.Offset%pageSize != 0 || d2.Offset%pageSize != 0 {
		t.Fatalf("windows not page aligned: %d, %d", d1.Offset, d2.Offset)
	}
	if d1.Offset == d2.Offset {
		t.Fatalf("windows overlap at %d", d1.Offset)
	}
	if d1.Length != 4096 || d2.Length != 4096 {
		t.Fatalf("window lengths %d, %d; wanted 4096", d1.Length, d2.Length)
	}
	_ = flipcall.PacketHeaderBytes // keep the flipcall ABI dependency visible
}

// TestControlPlaneLinux checks that the Linux control-plane probe reports
// every feature: these are unconditional parts of the Linux platform.
func TestControlPlaneLinux(t *testing.T) {
	cp := ProbeControlPlane()
	for _, ok := range []bool{cp.SeccompFilters, cp.Membarrier, cp.MemcgPressure, cp.Cgroups, cp.Namespaces, cp.Netlink, cp.HostInet} {
		if !ok {
			t.Errorf("ProbeControlPlane() = %+v; wanted every feature available on Linux", cp)
			return
		}
	}
}

// TestErrUnsupportedIsStandard pins the fail-closed sentinel to the stdlib
// value so all stub backends interoperate with errors.Is.
func TestErrUnsupportedIsStandard(t *testing.T) {
	if !errors.Is(ErrUnsupported, errors.ErrUnsupported) {
		t.Fatalf("ErrUnsupported must be errors.ErrUnsupported")
	}
}
