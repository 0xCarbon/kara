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

//go:build linux
// +build linux

package hostifc

import (
	"gvisor.dev/gvisor/pkg/fdchannel"
	"gvisor.dev/gvisor/pkg/flipcall"
	"gvisor.dev/gvisor/pkg/unet"
)

// defaultIPC implements IPC with the existing Linux host primitives:
// pkg/unet control sockets, pkg/fdchannel donation channels and pkg/flipcall
// packet windows. Nothing in this adapter changes how those packages behave;
// it only exposes them through the hostifc seams.
type defaultIPC struct{}

func defaultIPCImpl() IPC { return defaultIPC{} }

// ControlSocketFromFD implements IPC.ControlSocketFromFD.
func (defaultIPC) ControlSocketFromFD(fd int) (ControlSocket, error) {
	sock, err := unet.NewSocket(fd)
	if err != nil {
		return nil, err
	}
	return linuxControlSocket{sock}, nil
}

// NewControlSocketPair implements IPC.NewControlSocketPair.
func (defaultIPC) NewControlSocketPair() (ControlSocket, ControlSocket, error) {
	a, b, err := unet.SocketPair(false /* packet */)
	if err != nil {
		return nil, nil, err
	}
	return linuxControlSocket{a}, linuxControlSocket{b}, nil
}

// NewFDChannel implements IPC.NewFDChannel.
func (defaultIPC) NewFDChannel() (fdchannel.FDDonator, int, error) {
	fds, err := fdchannel.NewConnectedSockets()
	if err != nil {
		return nil, -1, err
	}
	return fdchannel.NewEndpoint(fds[0]), fds[1], nil
}

// NewPacketWindowAllocator implements IPC.NewPacketWindowAllocator.
func (defaultIPC) NewPacketWindowAllocator() (flipcall.WindowAllocator, error) {
	return flipcall.NewPacketWindowAllocator()
}

// linuxControlSocket adapts *unet.Socket to ControlSocket. Reader and Writer
// must return interface-satisfying values; unet's concrete SocketReader and
// SocketWriter satisfy StreamReader/StreamWriter through their embedded
// ControlMessage (see the compile-time assertions below).
type linuxControlSocket struct {
	*unet.Socket
}

// Reader implements ControlSocket.Reader.
func (s linuxControlSocket) Reader(blocking bool) StreamReader {
	r := s.Socket.Reader(blocking)
	return &r
}

// Writer implements ControlSocket.Writer.
func (s linuxControlSocket) Writer(blocking bool) StreamWriter {
	w := s.Socket.Writer(blocking)
	return &w
}

var (
	_ IPC           = defaultIPC{}
	_ ControlSocket = linuxControlSocket{}
	_ StreamReader  = (*unet.SocketReader)(nil)
	_ StreamWriter  = (*unet.SocketWriter)(nil)
)
