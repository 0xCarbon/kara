// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package egressgate_test exercises the stack-level egress gate
// (stack.Options.EgressGate, Oca #447): TCP connect admission, the L7
// prefix-mirroring hold/release state machine, UDP datagram gating, and the
// stock (nil gate) behavior.
package egressgate_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// mockGate implements stack.EgressGate with pluggable verdict functions and
// call recording.
type mockGate struct {
	tcpErr atomic.Pointer[tcpip.Error]
	udpErr atomic.Pointer[tcpip.Error]
	l7Fn   atomic.Pointer[func(prefix []byte) (bool, tcpip.Error)]

	tcpCalls atomic.Int32
	udpCalls atomic.Int32
	l7Calls  atomic.Int32

	mu      sync.Mutex
	prefixs [][]byte
}

func errPtr(err tcpip.Error) *tcpip.Error { return &err }

func (g *mockGate) CheckTCP(dst tcpip.FullAddress) tcpip.Error {
	g.tcpCalls.Add(1)
	if p := g.tcpErr.Load(); p != nil {
		return *p
	}
	return nil
}

func (g *mockGate) CheckUDP(dst tcpip.FullAddress) tcpip.Error {
	g.udpCalls.Add(1)
	if p := g.udpErr.Load(); p != nil {
		return *p
	}
	return nil
}

func (g *mockGate) CheckL7(dst tcpip.FullAddress, prefix []byte) (bool, tcpip.Error) {
	g.l7Calls.Add(1)
	g.mu.Lock()
	g.prefixs = append(g.prefixs, append([]byte(nil), prefix...))
	g.mu.Unlock()
	if p := g.l7Fn.Load(); p != nil {
		return (*p)(prefix)
	}
	return false, nil
}

func (g *mockGate) seenPrefixes() [][]byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([][]byte(nil), g.prefixs...)
}

var (
	clientAddr = tcpip.AddrFrom4([4]byte{10, 0, 0, 1})
	serverAddr = tcpip.AddrFrom4([4]byte{10, 0, 0, 2})
)

// gateStacks builds two stacks bridged by channel endpoints. Packets leaving
// the client stack (whose egress gate is gate) are pumped to the server stack
// and vice versa. clientSent counts packets forwarded client->server, i.e.
// everything that "left" the gated stack.
func gateStacks(t *testing.T, gate stack.EgressGate) (client, server *stack.Stack, clientSent *atomic.Int32) {
	t.Helper()
	clientSent = &atomic.Int32{}
	serverSent := &atomic.Int32{}

	client = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
		EgressGate:         gate,
	})
	server = stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})

	ce := channel.New(256, 1500, "")
	se := channel.New(256, 1500, "")
	if err := client.CreateNIC(1, ce); err != nil {
		t.Fatalf("client.CreateNIC: %s", err)
	}
	if err := server.CreateNIC(1, se); err != nil {
		t.Fatalf("server.CreateNIC: %s", err)
	}
	clientAddrWithPrefix := tcpip.ProtocolAddress{
		Protocol:          header.IPv4ProtocolNumber,
		AddressWithPrefix: clientAddr.WithPrefix(),
	}
	serverAddrWithPrefix := tcpip.ProtocolAddress{
		Protocol:          header.IPv4ProtocolNumber,
		AddressWithPrefix: serverAddr.WithPrefix(),
	}
	if err := client.AddProtocolAddress(1, clientAddrWithPrefix, stack.AddressProperties{}); err != nil {
		t.Fatalf("client.AddProtocolAddress: %s", err)
	}
	if err := server.AddProtocolAddress(1, serverAddrWithPrefix, stack.AddressProperties{}); err != nil {
		t.Fatalf("server.AddProtocolAddress: %s", err)
	}
	client.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	server.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// The channel endpoint hands out written packets with parsed header
	// views; inbound injection expects the raw wire packet in the payload
	// buffer, so re-wrapping is required when bridging two stacks.
	pump := func(from, to *channel.Endpoint, counter *atomic.Int32) {
		go func() {
			for {
				pkt := from.ReadContext(ctx)
				if pkt == nil {
					return
				}
				if counter != nil {
					counter.Add(1)
				}
				injected := stack.NewPacketBuffer(stack.PacketBufferOptions{
					Payload: pkt.ToBuffer(),
				})
				fromProto := pkt.NetworkProtocolNumber
				pkt.DecRef()
				to.InjectInbound(fromProto, injected)
			}
		}()
	}
	pump(ce, se, clientSent)
	pump(se, ce, serverSent)
	_ = serverSent
	return client, server, clientSent
}

func TestEgressGateTCPConnectDeniedNoSYN(t *testing.T) {
	g := &mockGate{}
	g.tcpErr.Store(errPtr(&tcpip.ErrConnectionRefused{}))
	client, _, clientSent := gateStacks(t, g)

	// A denied connect must fail with ECONNREFUSED and leave the stack
	// without emitting a single packet (no SYN, no ARP-equivalent).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := gonet.DialContextTCP(ctx, client, tcpip.FullAddress{
		NIC:  1,
		Addr: serverAddr,
		Port: 80,
	}, header.IPv4ProtocolNumber)
	if err == nil {
		t.Fatal("connect with denying gate unexpectedly succeeded")
	}
	// gonet maps a refused connect to an opError stringifying the tcpip
	// error, so assert on the mapped message.
	if !isConnRefused(err) {
		t.Fatalf("connect error = %v, want ECONNREFUSED", err)
	}
	if n := g.tcpCalls.Load(); n != 1 {
		t.Errorf("gate.CheckTCP calls = %d, want 1", n)
	}
	// Give any would-be deferred send a chance to run before asserting.
	time.Sleep(100 * time.Millisecond)
	if n := clientSent.Load(); n != 0 {
		t.Errorf("packets left the gated stack = %d, want 0 (no SYN must escape)", n)
	}
}

func TestEgressGateTCPConnectAllowed(t *testing.T) {
	g := &mockGate{} // allow everything
	client, server, _ := gateStacks(t, g)

	ln, err := gonet.ListenTCP(server, tcpip.FullAddress{Addr: serverAddr, Port: 80}, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatalf("ListenTCP: %v", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := gonet.DialContextTCP(ctx, client, tcpip.FullAddress{NIC: 1, Addr: serverAddr, Port: 80}, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatalf("DialContextTCP with permissive gate: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	peer, err := ln.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	defer peer.Close()
	peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, 5)
	if _, err := io.ReadFull(peer, got); err != nil {
		t.Fatalf("peer read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("peer read %q, want %q", got, "hello")
	}
	if n := g.tcpCalls.Load(); n != 1 {
		t.Errorf("gate.CheckTCP calls = %d, want 1", n)
	}
	if n := g.l7Calls.Load(); n != 1 {
		t.Errorf("gate.CheckL7 calls = %d, want 1 (allow verdict on first payload)", n)
	}
}

// TestEgressGateTCPL7HoldThenRelease drives the L7 sniffing state machine:
// bytes written while the gate needs more prefix are held in the send queue
// (nothing observable reaches the peer), then released together once the
// gate issues an allow verdict.
func TestEgressGateTCPL7HoldThenRelease(t *testing.T) {
	const full = "GET /index.html HTTP/1.1\r\n\r\n"
	g := &mockGate{}
	needMore := func(prefix []byte) (bool, tcpip.Error) {
		return len(prefix) < len(full), nil // allow once the full request is mirrored
	}
	g.l7Fn.Store(&needMore)
	client, server, _ := gateStacks(t, g)

	ln, err := gonet.ListenTCP(server, tcpip.FullAddress{Addr: serverAddr, Port: 80}, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatalf("ListenTCP: %v", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := gonet.DialContextTCP(ctx, client, tcpip.FullAddress{NIC: 1, Addr: serverAddr, Port: 80}, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatalf("DialContextTCP: %v", err)
	}
	defer conn.Close()
	peer, err := ln.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	defer peer.Close()

	// First (partial) write: must be accepted by the socket but HELD.
	if n, err := conn.Write([]byte("GET ")); err != nil || n != 4 {
		t.Fatalf("held write = (%d, %v), want (4, nil)", n, err)
	}
	peer.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	buf := make([]byte, len(full))
	if n, err := peer.Read(buf); err == nil {
		t.Fatalf("peer read %d bytes while flow must be held; want timeout", n)
	}
	if n := g.l7Calls.Load(); n != 1 {
		t.Errorf("gate.CheckL7 calls after held write = %d, want 1", n)
	}

	// Completing the mirrored prefix yields an allow verdict and releases
	// the held bytes together with the new ones.
	if n, err := conn.Write([]byte(full[4:])); err != nil || n != len(full)-4 {
		t.Fatalf("releasing write = (%d, %v)", n, err)
	}
	peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(io.LimitReader(peer, int64(len(full))))
	if err != nil {
		t.Fatalf("peer read after release: %v", err)
	}
	if string(got) != full {
		t.Fatalf("peer read %q, want %q", got, full)
	}

	// The gate must have seen exactly the accumulated prefixes.
	prefixs := g.seenPrefixes()
	wantLens := []int{4, len(full)}
	if len(prefixs) != len(wantLens) {
		t.Fatalf("gate prefixes = %d, want %d", len(prefixs), len(wantLens))
	}
	for i, want := range wantLens {
		if len(prefixs[i]) != want {
			t.Errorf("prefix[%d] len = %d, want %d (%q)", i, len(prefixs[i]), want, prefixs[i])
		}
	}
}

// TestEgressGateTCPL7DeniedReset verifies a deny verdict from CheckL7 fails
// the write and resets the connection, preventing any held byte from
// escaping.
func TestEgressGateTCPL7DeniedReset(t *testing.T) {
	g := &mockGate{}
	deny := func(prefix []byte) (bool, tcpip.Error) {
		return false, &tcpip.ErrConnectionRefused{}
	}
	g.l7Fn.Store(&deny)
	client, server, _ := gateStacks(t, g)

	ln, err := gonet.ListenTCP(server, tcpip.FullAddress{Addr: serverAddr, Port: 80}, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatalf("ListenTCP: %v", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := gonet.DialContextTCP(ctx, client, tcpip.FullAddress{NIC: 1, Addr: serverAddr, Port: 80}, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatalf("DialContextTCP: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("evil")); err == nil {
		t.Fatal("write with denying L7 gate unexpectedly succeeded")
	}
	// After the reset the peer sees EOF or RST, never the payload.
	peer, err := ln.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	defer peer.Close()
	// The acceptance criterion is that the payload never escapes: any
	// successful or non-empty read fails the test. Zero-byte errors are
	// acceptable outcomes (EOF after the deny purge, or ECONNRESET from the
	// RST); a read-deadline expiry with zero bytes is tolerated to avoid
	// FIN/RST arrival races.
	peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	if n, err := peer.Read(buf); err == nil || n > 0 {
		t.Fatalf("payload escaped the denying L7 gate: peer read %d bytes (%q)", n, buf[:n])
	}
}

func TestEgressGateUDPWriteDenied(t *testing.T) {
	g := &mockGate{}
	g.udpErr.Store(errPtr(&tcpip.ErrConnectionRefused{}))
	client, _, clientSent := gateStacks(t, g)

	raddr := tcpip.FullAddress{Addr: serverAddr, Port: 53}
	conn, err := gonet.DialUDP(client, nil, &raddr, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatalf("DialUDP: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("q")); err == nil {
		t.Fatal("UDP write with denying gate unexpectedly succeeded")
	}
	if n := g.udpCalls.Load(); n != 1 {
		t.Errorf("gate.CheckUDP calls = %d, want 1", n)
	}
	time.Sleep(100 * time.Millisecond)
	if n := clientSent.Load(); n != 0 {
		t.Errorf("packets left the gated stack = %d, want 0", n)
	}
}

// TestEgressGateUDPWriteToDenied drives the unconnected sendto path
// (WriteTo with an explicit destination) through the gate: the datagram
// must be refused before routing and zero packets must leave the stack.
func TestEgressGateUDPWriteToDenied(t *testing.T) {
	g := &mockGate{}
	g.udpErr.Store(errPtr(&tcpip.ErrConnectionRefused{}))
	client, _, clientSent := gateStacks(t, g)

	// Unconnected socket: destination supplied per write.
	conn, err := gonet.DialUDP(client, nil, nil, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatalf("DialUDP: %v", err)
	}
	defer conn.Close()
	dst := &net.UDPAddr{IP: serverAddr.AsSlice(), Port: 53}
	if _, err := conn.WriteTo([]byte("q"), dst); err == nil {
		t.Fatal("UDP sendto with denying gate unexpectedly succeeded")
	}
	if n := g.udpCalls.Load(); n != 1 {
		t.Errorf("gate.CheckUDP calls = %d, want 1", n)
	}
	time.Sleep(100 * time.Millisecond)
	if n := clientSent.Load(); n != 0 {
		t.Errorf("packets left the gated stack = %d, want 0", n)
	}
}

func TestEgressGateUDPWriteAllowed(t *testing.T) {
	g := &mockGate{} // allow everything
	client, server, _ := gateStacks(t, g)

	laddr := tcpip.FullAddress{Addr: serverAddr, Port: 53}
	peer, err := gonet.DialUDP(server, &laddr, nil, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatalf("peer DialUDP: %v", err)
	}
	defer peer.Close()

	raddr := tcpip.FullAddress{Addr: serverAddr, Port: 53}
	conn, err := gonet.DialUDP(client, nil, &raddr, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatalf("DialUDP: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("query")); err != nil {
		t.Fatalf("UDP write with permissive gate: %v", err)
	}
	peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 16)
	n, err := peer.Read(buf)
	if err != nil {
		t.Fatalf("peer read: %v", err)
	}
	if string(buf[:n]) != "query" {
		t.Fatalf("peer read %q, want %q", buf[:n], "query")
	}
}

// TestEgressGateNilGateStockBehavior: with no gate installed, connects and
// writes behave stock (regression guard for the nil-gate fast path).
func TestEgressGateNilGateStockBehavior(t *testing.T) {
	client, server, _ := gateStacks(t, nil)

	ln, err := gonet.ListenTCP(server, tcpip.FullAddress{Addr: serverAddr, Port: 80}, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatalf("ListenTCP: %v", err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := gonet.DialContextTCP(ctx, client, tcpip.FullAddress{NIC: 1, Addr: serverAddr, Port: 80}, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatalf("DialContextTCP without gate: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("stock")); err != nil {
		t.Fatalf("write without gate: %v", err)
	}
	peer, err := ln.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	defer peer.Close()
	peer.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, 5)
	if _, err := io.ReadFull(peer, got); err != nil {
		t.Fatalf("peer read: %v", err)
	}
	if string(got) != "stock" {
		t.Fatalf("peer read %q, want %q", got, "stock")
	}
}

func isConnRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	return strings.Contains(err.Error(), (&tcpip.ErrConnectionRefused{}).String())
}

// TestEgressGateConnectStatePrecedence pins Linux errno precedence over the
// gate on re-connect: once a socket is connecting or connected, Connect must
// return EALREADY/EISCONN (ErrAlreadyConnecting/ErrAlreadyConnected) even when
// the gate is denying or dead — the gate may only refuse NEW flows.
func TestEgressGateConnectStatePrecedence(t *testing.T) {
	g := &mockGate{} // allow the first connect
	client, server, _ := gateStacks(t, g)

	// A real listener so the first connect can reach ESTABLISHED.
	ln, err := gonet.ListenTCP(server, tcpip.FullAddress{Addr: serverAddr, Port: 80}, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatalf("ListenTCP: %v", err)
	}
	defer ln.Close()

	var wq waiter.Queue
	ep, terr := client.NewEndpoint(tcp.ProtocolNumber, header.IPv4ProtocolNumber, &wq)
	if terr != nil {
		t.Fatalf("NewEndpoint: %v", terr)
	}
	defer ep.Close()

	entry, notifyCh := waiter.NewChannelEntry(waiter.WritableEvents)
	wq.EventRegister(&entry)
	defer wq.EventUnregister(&entry)

	addr := tcpip.FullAddress{NIC: 1, Addr: serverAddr, Port: 80}
	if err := ep.Connect(addr); err == nil {
		t.Fatal("non-blocking Connect returned nil, want ErrConnectStarted")
	} else if _, ok := err.(*tcpip.ErrConnectStarted); !ok {
		t.Fatalf("first Connect: %v, want ErrConnectStarted", err)
	}
	if n := g.tcpCalls.Load(); n != 1 {
		t.Fatalf("gate.CheckTCP calls after first connect = %d, want 1", n)
	}

	// Flip the gate to deny BEFORE the flow is established: every re-connect
	// must report socket state (EALREADY, then the one-shot success
	// notification / EISCONN), never the gate's ECONNREFUSED.
	g.tcpErr.Store(errPtr(&tcpip.ErrConnectionRefused{}))
	deadline := time.Now().Add(5 * time.Second)
	established := false
	for !established {
		err := ep.Connect(addr)
		switch err.(type) {
		case *tcpip.ErrAlreadyConnected:
			established = true
		case *tcpip.ErrAlreadyConnecting:
			if time.Now().After(deadline) {
				t.Fatal("connection never established")
			}
			select {
			case <-notifyCh:
			case <-time.After(100 * time.Millisecond):
			}
		case nil:
			// Netstack emulates Linux non-blocking semantics: the first
			// re-connect after establishment returns the pending success
			// notification once; the next one must be EISCONN.
			established = true
		default:
			t.Fatalf("re-Connect with denying gate: %v, want ErrAlreadyConnecting/ErrAlreadyConnected (state must precede gate)", err)
		}
	}
	// Final proof: EISCONN with the gate denying, and the gate never
	// consulted again for the existing flow.
	if err := ep.Connect(addr); err == nil {
		t.Fatal("re-Connect after notification returned nil, want ErrAlreadyConnected")
	} else if _, ok := err.(*tcpip.ErrAlreadyConnected); !ok {
		t.Fatalf("final re-Connect with denying gate: %v, want ErrAlreadyConnected", err)
	}
	if n := g.tcpCalls.Load(); n != 1 {
		t.Fatalf("gate.CheckTCP calls = %d, want 1 (state validation must precede the gate)", n)
	}
}
