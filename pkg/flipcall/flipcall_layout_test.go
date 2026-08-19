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

package flipcall

import (
	"testing"
	"unsafe"
)

// TestPacketWindowHeaderLayout pins the 16-byte packet window header, which
// is part of the LISAFS channel wire format (pkg/lisafs/ABI.md §Channels).
// The header is NATIVE endian: a 32-bit connection state word, a 32-bit
// datagram length, and 8 reserved bytes. Consumers (lisafs channels) place
// their own header at window offset 16.
func TestPacketWindowHeaderLayout(t *testing.T) {
	if got, want := PacketHeaderBytes, 16; got != want {
		t.Fatalf("PacketHeaderBytes = %d; want %d", got, want)
	}

	alloc, err := NewPacketWindowAllocator()
	if err != nil {
		t.Fatalf("NewPacketWindowAllocator: %v", err)
	}
	defer alloc.Destroy()

	pwd, err := alloc.Allocate(PacketWindowLengthForDataCap(64))
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	var ep Endpoint
	if err := ep.Init(ClientSide, pwd); err != nil {
		t.Fatalf("Endpoint.Init: %v", err)
	}
	defer ep.Destroy()

	// The connection-state word is the first word of the window...
	if got := uintptr(unsafe.Pointer(ep.connState())); got != ep.packet {
		t.Errorf("connState word at window offset %d; want 0", got-ep.packet)
	}
	// ...followed by the datagram length word...
	if got := uintptr(unsafe.Pointer(ep.dataLen())); got != ep.packet+4 {
		t.Errorf("dataLen word at window offset %d; want 4", got-ep.packet)
	}
	// ...with the datagram region beginning after the 16-byte header.
	data := ep.Data()
	if got := uintptr(unsafe.Pointer(&data[0])); got != ep.packet+uintptr(PacketHeaderBytes) {
		t.Errorf("datagram region at window offset %d; want %d", got-ep.packet, PacketHeaderBytes)
	}
	// The data capacity is the window minus the header.
	if got, want := uint32(len(data)), ep.dataCap; got != want {
		t.Errorf("len(Data()) = %d; want dataCap %d", got, want)
	}
	if pwd.Length < PacketHeaderBytes+len(data) {
		t.Errorf("window length %d < header+dataCap %d", pwd.Length, PacketHeaderBytes+len(data))
	}
}

// TestSleeperSeam pins the Sleeper seam contract against the Linux futex
// implementation (seam.go): Wake with no waiters succeeds, and Wait returns
// without error when the word no longer equals cur (the documented spurious/
// EAGAIN tolerance that callers rely on).
func TestSleeperSeam(t *testing.T) {
	alloc, err := NewPacketWindowAllocator()
	if err != nil {
		t.Fatalf("NewPacketWindowAllocator: %v", err)
	}
	defer alloc.Destroy()

	pwd, err := alloc.Allocate(PacketWindowLengthForDataCap(64))
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	var ep Endpoint
	if err := ep.Init(ClientSide, pwd); err != nil {
		t.Fatalf("Endpoint.Init: %v", err)
	}
	defer ep.Destroy()

	s := ep.Sleeper()
	if s == nil {
		t.Fatal("Endpoint.Sleeper() returned nil")
	}
	// Waking zero/one waiter with nobody blocked must succeed.
	if err := s.Wake(1); err != nil {
		t.Errorf("Sleeper.Wake(1) with no waiters: %v", err)
	}
	// connState starts at the client-active state for a ClientSide
	// endpoint; waiting on the current value would block, so change the
	// word first: Wait(cur=old) must return immediately (EAGAIN tolerated).
	old := ep.connState().Load()
	ep.connState().Store(ep.inactiveState)
	if err := s.Wait(old); err != nil {
		t.Errorf("Sleeper.Wait(cur) after word change: %v; want nil (spurious/EAGAIN tolerated)", err)
	}
}

// TestWindowAllocatorSeam pins the WindowAllocator seam (seam.go) against the
// Linux memfd implementation: successive allocations are disjoint, page
// aligned, and backed by one sealed file.
func TestWindowAllocatorSeam(t *testing.T) {
	alloc, err := NewPacketWindowAllocator()
	if err != nil {
		t.Fatalf("NewPacketWindowAllocator: %v", err)
	}
	defer alloc.Destroy()

	if fd := alloc.FD(); fd < 0 {
		t.Errorf("allocator FD = %d; want >= 0", fd)
	}
	var prev PacketWindowDescriptor
	for i, size := range []int{1, 4097, 1 << 20} {
		pwd, err := alloc.Allocate(size)
		if err != nil {
			t.Fatalf("Allocate(%d): %v", size, err)
		}
		if pwd.FD != alloc.FD() {
			t.Errorf("Allocate(%d).FD = %d; want %d", size, pwd.FD, alloc.FD())
		}
		if pwd.Length < size {
			t.Errorf("Allocate(%d).Length = %d; want >= %d", size, pwd.Length, size)
		}
		if pwd.Offset%int64(pageSize) != 0 {
			t.Errorf("Allocate(%d).Offset = %d; want page aligned", size, pwd.Offset)
		}
		if i > 0 && pwd.Offset != prev.Offset+int64(prev.Length) {
			t.Errorf("Allocate(%d).Offset = %d; want %d (disjoint, contiguous)", size, pwd.Offset, prev.Offset+int64(prev.Length))
		}
		prev = pwd
	}
}
