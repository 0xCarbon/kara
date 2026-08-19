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

// ControlPlane reports which Linux-only host control-plane features are
// available on this host. These features sandbox and shape the sentry
// process itself; a non-Linux host must replace each of them at the VM or
// job boundary instead (see pkg/sentry/platform/platform-seam.md for the
// per-feature mapping).
//
// A false field means the feature is absent AND its sentry-side packages
// fail closed (return errors.ErrUnsupported or natural errors) rather than
// changing behavior: e.g. seccomp.SetFilter on non-Linux hosts returns
// ErrUnsupported, and hostmm.Probe reports no membarrier support. This lets
// the sentry core compile and boot on non-Linux hosts with those features
// disabled, instead of requiring sentry-wide ifdefs.
type ControlPlane struct {
	// SeccompFilters: installing host seccomp filters (prctl(2)
	// PR_SET_SECCOMP + seccomp(2)). Linux: pkg/seccomp.
	SeccompFilters bool

	// Membarrier: the membarrier(2) syscall. Linux: pkg/sentry/hostmm.
	Membarrier bool

	// MemcgPressure: cgroup memory-pressure notifications on the sentry's
	// own memory cgroup (Linux: pkg/sentry/hostmm
	// NotifyCurrentMemcgPressureCallback).
	MemcgPressure bool

	// Cgroups: host cgroup filesystems for resource limiting of the sandbox
	// and gofer processes (Linux: runsc/cgroup).
	Cgroups bool

	// Namespaces: unshare(2)/setns(2) mount/PID/network namespaces
	// (Linux: runsc launcher).
	Namespaces bool

	// Netlink: host netlink sockets, e.g. rtnetlink for interface
	// configuration (Linux: pkg/sentry/socket/hostinet).
	Netlink bool

	// HostInet: host internet-domain sockets usable by the sentry's
	// hostinet socket provider. When false, networking must come from
	// netstack on a host-agnostic dataplane.
	HostInet bool
}

// ProbeControlPlane reports the host's control-plane capabilities. On Linux
// every feature listed above is an unconditional part of the platform and
// reported as available; per-operation failures (e.g. a kernel without
// MEMBARRIER_CMD_GLOBAL) still surface from the underlying packages at use
// time. On non-Linux hosts every feature is reported as absent.
func ProbeControlPlane() ControlPlane {
	return probeControlPlane()
}
