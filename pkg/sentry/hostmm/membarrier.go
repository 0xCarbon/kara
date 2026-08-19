// Copyright 2020 The gVisor Authors.
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

package hostmm

// HostMemBarrier provides access to the host membarrier(2) operations that the
// calling process has been verified to support. It is obtained from Probe.
type HostMemBarrier struct {
	// global is whether the host supports MEMBARRIER_CMD_GLOBAL.
	global bool

	// privateExpedited is true if we registered for
	// `MEMBARRIER_CMD_PRIVATE_EXPEDITED`.
	privateExpedited bool
}

// HaveGlobalMemoryBarrier returns true if GlobalMemoryBarrier is supported.
func (h HostMemBarrier) HaveGlobalMemoryBarrier() bool {
	return h.global
}

// HaveProcessMemoryBarrier returns true if ProcessMemoryBarrier is supported
// and registration succeeded.
func (h HostMemBarrier) HaveProcessMemoryBarrier() bool {
	return h.privateExpedited
}

// Probe asynchronously determines host `membarrier(2)` support and, if
// `probePrivateExpedited` is true and the host supports it, registers for
// MEMBARRIER_CMD_PRIVATE_EXPEDITED.
// Returns a channel on which the resulting HostMemBarrier is sent back.
// This is meant to be used in Platform implementation constructors, which
// mus run before seccomp filters installation.
// Runs in the background to allow further platform initialization to proceed
// while we determine this, which can take tens of milliseconds.
func Probe(probePrivateExpedited bool) <-chan HostMemBarrier {
	ch := make(chan HostMemBarrier, 1)
	go func() {
		ch <- probe(probePrivateExpedited)
		close(ch)
	}()
	return ch
}
