# flipcall host-primitive seam (wave-03; consumed by wave-04/05)

`pkg/flipcall` implements Fast Local IPC between mutually-distrusting
processes. Today it is Linux-only (futex(2), sealed memfd, SCM_RIGHTS). This
document specifies the narrow seams behind which those host primitives hide,
so non-Linux backends (wave-05: POSIX shm, Mach ports, named pipes) can land
without sentry-wide ifdefs. **Wave-03 ships interfaces, stubs and layout pins
only — the Linux implementation is unchanged.**

## The seams (seam.go, fdchannel)

| Seam | Interface | Linux implementation |
|------|-----------|----------------------|
| Control transfer | `flipcall.Sleeper { Wake(n int32) error; Wait(cur uint32) error }` | futex(2) `FUTEX_WAKE`/`FUTEX_WAIT` on the window's connection-state word (`futex_linux.go`, exposed via `Endpoint.Sleeper()`) |
| Window allocation | `flipcall.WindowAllocator { Allocate(size) (PacketWindowDescriptor, error); FD() int; Destroy() }` | sealed memfd (`F_SEAL_SHRINK|F_SEAL_SEAL`), page-aligned bump allocation (`packet_window.go`) |
| FD donation | `fdchannel.FDDonator { SendFD; RecvFD; RecvFDNonblock; Shutdown; Destroy }` | `AF_UNIX SOCK_SEQPACKET` socketpair, one `SCM_RIGHTS` cmsg per FD (`fdchannel_unsafe.go`) |

`Sleeper` semantics are futex-like by contract: `Wake` with no waiters
succeeds; `Wait(cur)` tolerates spurious returns (`EAGAIN`/`EINTR`) and
callers must re-check the word in a loop. `Allocate` must return page-aligned
disjoint windows backed by one descriptor transmissible to the peer process.
`FDDonator` preserves per-FD message boundaries and ordering; a failed
`SendFD` loses that FD and all subsequent ones (consumers treat FDs as
supplementary data, never the RPC itself — see lisafs `channel.sendFDs`).

## Packet window header (16 bytes, NATIVE endian — part of the LISAFS ABI)

```
offset  size  field
0       4     connection state word (u32): csClientActive | csServerActive | csShutdown
4       4     datagram length (u32)
8       8     reserved (zeros)
16      ...   datagram region: consumer header (lisafs: 4 B channelHeader) + payload
```

Pinned by `flipcall_layout_test.go` (`connState@0`, `dataLen@4`,
`Data()@16`, `PacketHeaderBytes == 16`) and by `lisafs`'s
`abi_conformance_test.go` for the consumer header. **Backends must keep this
layout and endianness**: it is shared memory, both peers read it with their
native byte order, and the LISAFS wire ABI (pkg/lisafs/ABI.md) depends on it.

## Non-Linux hosts today

`futex_stub.go` (`//go:build !linux`) makes the package compile on non-Linux
hosts and fail closed: every control-transfer operation returns
`ErrUnsupported` (`ShutdownError` after a local `Shutdown()`). Word
accessors (`connState`, `dataLen`) and window layout code are
host-independent. Verified: `GOOS=darwin go build ./pkg/flipcall/` compiles
with these stubs (plus the `memutil.CreateMemFD` non-Linux stub). Known
remaining gaps, tracked for wave-04/05: `pkg/memutil`'s mmap paths and
`pkg/hostarch` protection bits are not windows-portable (windows builds are
blocked there, not at this seam), and `fdchannel` needs `SOCK_CLOEXEC`
equivalents on darwin. `go vet` flags flipcall_unsafe.go's raw-window
pointer pattern (uintptr↔unsafe.Pointer) on all platforms equally; this is
inherent to mapping a window shared with an untrusted peer and predates the
seam.

## What a wave-05 backend must provide

1. A `Sleeper`: Mach port semantics (darwin) or a POSIX shim on the shared
   word (e.g. `pthread_cond`-style protocol with a per-window wait channel)
   implementing the spurious-tolerance contract above.
2. A `WindowAllocator`: POSIX `shm_open` or a Mach VM copy-on-write region,
   with a truncation-safety equivalent of memfd seals (the peer must never be
   able to shrink the mapping under a live window — SIGBUS is not an option).
3. An `FDDonator`: whatever carries descriptors across the host boundary
   (Mach ports carry send rights; named pipes need an SCM_RIGHTS-equivalent
   side channel).
4. A factory injection point so `lisafs.Connection.createChannel` can be
   built per host without callers knowing the backend (today it calls
   `flipcall.NewPacketWindowAllocator()` directly; wave-04's platform-seam
   work owns that routing).

Indirection cost note: the Linux hot path still calls the concrete
`futex*ConnState` methods; the interface adapters (`futexSleeper`) exist for
seam consumers, so there is no added call overhead in the RPC path.
