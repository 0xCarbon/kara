# LISAFS Wire Protocol ABI — Version 1 (frozen)

This document is the byte-level contract of the LISAFS protocol implemented by
`pkg/lisafs` (client) and served by `runsc/fsgofer` and third-party gofers
(the reference external consumer is Oca's hand-rolled server in
`oca/gofer/lisafs_wire.go`). It is **machine-checked**: `abi_conformance_test.go`
pins every size and offset asserted below and replays golden bytes produced by
the real `go_marshal` code in `testdata/golden/`. If this document and the code
disagree, the code is wrong *or the ABI was broken* — either way CI must fail.

## Versioning policy

* `LISAFSWireABI = 1`. Version 1 is frozen at MIDs 0–33 (the table below).
* MIDs **0–255 are reserved**. New messages may only be **appended** with the
  next free MID; existing MIDs must never be renumbered, reused, or removed.
* Existing message layouts must never change — including padding bytes, which
  are part of the wire format. New fields require a new MID (a new message
  version is a new message).
* MIDs ≥ 256 are reserved for connection-local dynamic messages (the server
  advertises them via `MountResp.SupportedMs`; see `connection_test.go`'s
  `dynamicMsgID` pattern).
* The MID table below is **generated** from `message.go` by
  `go generate ./...` (`abi_gen`). Editing `message.go`'s MID block without
  regenerating this file fails `abi_conformance_test`.
* An incompatible change (if ever needed) requires a new negotiation mechanism
  first and a `LISAFSWireABI` bump; version 1 peers never coexist with it on
  one connection.

## Common encoding rules (framing)

* All integers are **little-endian**, fixed width (`uint16`/`uint32`/`uint64`).
* Structures serialize fields in declaration order with **natural alignment**
  enforced by *explicit padding fields* (unnamed `_` fields in the Go
  declarations). **Padding bytes are transmitted** (senders write zeros;
  receivers must ignore them). Example: `OpenAtReq` is 16 bytes on the wire:
  `u64 FD + u32 Flags + 4 pad`.
* `SizedString` = `u16 length + raw bytes` (not NUL terminated, not u32).
* `StringArray` = `u16 count + count × SizedString`.
* `FdArray` = `u16 count + count × u64 FDID`.
* Dynamic trailing buffers (e.g. `PWriteReq.Buf`) follow their length field
  with **no trailing padding** (`PWriteReq` = `u64 Offset + u64 FD +
  u32 NumBytes + NumBytes bytes`, size `20 + len(Buf)`; contrast the fixed
  `PReadReq` which carries 4 trailing pad bytes).
* A message = communicator header + payload. `MaxMessageSize` (negotiated at
  Mount, `u32`) bounds the **payload** length on both directions; requests
  exceeding it fail with `EIO` before dispatch, responses exceeding it are
  converted to `EIO`. This package's default is `MaxMessageSize()` (a huge
  page minus a page, so the channel window is huge-page backed); servers may
  advertise smaller bounds (Oca uses 1 MiB). A client MUST NOT send a payload
  larger than the server-advertised bound.
* Unmarshalling must always use the `CheckedUnmarshal` variants: a hostile
  peer controls every byte.

## Communicators

A connection multiplexes one control socket and up to `maxChannels` fast
channels. The wire format of a message depends on the communicator carrying it.

### Control socket (UDS, `SOCK_SEQPACKET`)

```
offset  size  field
0       4     payloadLen   (u32, excludes this 8-byte header)
4       2     message      (u16 MID)
6       2     padding      (zeros)
8       ...   payload (payloadLen bytes)
```

File descriptors are donated as `SCM_RIGHTS` ancillary data on the same
socket, appended after the message that produces them.

### Channel (flipcall packet window + fdchannel socket)

A channel is a flipcall packet window: a sealed shared-memory (`memfd`) region
plus a `SOCK_SEQPACKET` `fdchannel` socket for FD donation. The window layout:

```
offset  size  field
0       4     flipcall connection state (u32, NATIVE endian, managed by flipcall)
4       4     flipcall datagram length  (u32, NATIVE endian, managed by flipcall)
8       8     reserved (zeros)
16      ...   datagram: [ channel header | payload ]
```

The datagram region carries a 4-byte LISAFS channel header before the payload:

```
offset  size  field
16      2     message (u16 MID)
18      1     numFDs  (u8 count of FDs donated on the fdchannel socket)
19      1     padding (zero)
20      ...   payload
```

Each donated FD arrives as its own `SCM_RIGHTS` message on the channel's
fdchannel socket, in order. The window is sized
`flipcall.PacketHeaderBytes (16) + 4 (channel header) + MaxMessageSize`.
Exchange is one-shot RPC (`SendRecvFast`): the active side writes header +
payload and transfers control; the peer replies in the same window.

## Connection lifecycle and handshake

1. **Connect.** The client connects a UDS to the server (`Mount` is the only
   RPC allowed before handshake completes).
2. **Mount (MID 1).** Empty request. Response: `Inode Root` + `u32
   MaxMessageSize` + `u16 numSupported` + `numSupported × u16 MID`
   (`SupportedMs`). The server may donate a host FD for directfs mounts.
3. **Channel (MID 2), repeated.** Empty request *on the control socket*.
   Response: `ChannelResp { i64 dataOffset; u64 dataLength }` (16 bytes)
   locating the datagram region inside the donated window, plus two donated
   FDs: the shared-memory file (memfd) and the channel's fdchannel socket.
   The client mmaps the window (`MAP_SHARED`) and completes the flipcall
   connect handshake. **Channels are mandatory for boot: the sentry's gofer
   client requires at least one channel to start a mount**
   (`pkg/sentry/fsimpl/gofer/gofer.go` `StartChannels`) — a server that
   refuses all Channel RPCs cannot back a sandbox filesystem.
4. **Steady state.** Non-`Mount`/`Channel` RPCs run over channels
   (round-robin); the control socket remains for Channel creation and as a
   fallback.
5. **Teardown.** Closing the control socket tears the connection down; the
   server shuts down and destroys all channels. `maxChannels =
   clamp(GOMAXPROCS, 2, 4)` per connection (this package's rule, applied by
   both ends); Channel requests beyond the cap fail with `ENOMEM`.

## Error handling

Failures are reported in-band: response MID `0` (`Error`) with a 4-byte
`u32 errno` payload. Mapping: unsupported/unknown MID → `EOPNOTSUPP`; payload
over `MaxMessageSize` → `EIO`; handler error → the extracted errno; handler
panic → `EREMOTEIO` (the connection survives). A failed RPC must leave no
partial server state (FDs allocated for a failed response are rolled back and
closed; where an operation is partially applied, the response documents it —
e.g. `SetStatResp.FailureMask` reports per-field failures instead of failing).

## MID table

<!-- BEGIN GENERATED MID TABLE (go:generate go run ./abi_gen) -->

| MID | Name    | Purpose |
|----:|---------|---------|
|   0 | `Error`  | Error is only used in responses to pass errors to client. |
|   1 | `Mount`  | Mount is used to establish connection between the client and server mount point. lisafs requires that the client makes a successful Mount RPC before making other RPCs. |
|   2 | `Channel`| Channel requests to start a new communicational channel. |
|   3 | `FStat`  | FStat requests the stat(2) results for a specified file. |
|   4 | `SetStat`| SetStat requests to change file attributes. Note that there is no one corresponding Linux syscall. This is a conglomeration of fchmod(2), fchown(2), ftruncate(2) and futimesat(2). |
|   5 | `Walk`   | Walk requests to walk the specified path starting from the specified directory. Server-side path traversal is terminated preemptively on symlinks entries because they can cause non-linear traversal. |
|   6 | `WalkStat`| WalkStat is the same as Walk, except the following differences: * If the first path component is "", then it also returns stat results for the directory where the walk starts. * Does not return Inode, just the Stat results for each path component. |
|   7 | `OpenAt` | OpenAt is analogous to openat(2). It does not perform any walk. It merely duplicates the control FD with the open flags passed. |
|   8 | `OpenCreateAt`| OpenCreateAt is analogous to openat(2) with O_CREAT|O_EXCL added to flags. It also returns the newly created file inode. |
|   9 | `Close`  | Close is analogous to close(2) but can work on multiple FDs. |
|  10 | `FSync`  | FSync is analogous to fsync(2) but can work on multiple FDs. |
|  11 | `PWrite` | PWrite is analogous to pwrite(2). |
|  12 | `PRead`  | PRead is analogous to pread(2). |
|  13 | `MkdirAt`| MkdirAt is analogous to mkdirat(2). |
|  14 | `MknodAt`| MknodAt is analogous to mknodat(2). |
|  15 | `SymlinkAt`| SymlinkAt is analogous to symlinkat(2). |
|  16 | `LinkAt` | LinkAt is analogous to linkat(2). |
|  17 | `FStatFS`| FStatFS is analogous to fstatfs(2). |
|  18 | `FAllocate`| FAllocate is analogous to fallocate(2). |
|  19 | `ReadLinkAt`| ReadLinkAt is analogous to readlinkat(2). |
|  20 | `Flush`  | Flush cleans up the file state. Its behavior is implementation dependent and might not even be supported in server implementations. |
|  21 | `Connect`| Connect is loosely analogous to connect(2). |
|  22 | `UnlinkAt`| UnlinkAt is analogous to unlinkat(2). |
|  23 | `RenameAt`| RenameAt is loosely analogous to renameat(2). |
|  24 | `Getdents64`| Getdents64 is analogous to getdents64(2). |
|  25 | `FGetXattr`| FGetXattr is analogous to fgetxattr(2). |
|  26 | `FSetXattr`| FSetXattr is analogous to fsetxattr(2). |
|  27 | `FListXattr`| FListXattr is analogous to flistxattr(2). |
|  28 | `FRemoveXattr`| FRemoveXattr is analogous to fremovexattr(2). |
|  29 | `BindAt` | BindAt is analogous to bind(2). |
|  30 | `Listen` | Listen is analogous to listen(2). |
|  31 | `Accept` | Accept is analogous to accept4(2). |
|  32 | `ConnectWithCreds`| ConnectWithCreds is analogous to connect(2) but it asks the server to connect with the provided effective uid/gid. |
|  33 | `RenameAt2`| RenameAt2 is loosely analogous to renameat2(2). |

<!-- END GENERATED MID TABLE -->

## Message layouts

Sizes/offsets in decimal bytes; every row is asserted by
`abi_conformance_test.go`. `pad` rows are explicit padding fields (zeros on
the wire). Dynamic fields are marked `dyn`.

### Common types

| Type | Size | Layout |
|------|-----:|--------|
| `MID` | 2 | `u16` |
| `FDID` | 8 | `u64` (0 = `InvalidFDID`; allocated from 1 sequentially per connection) |
| `UID`/`GID` | 4 | `u32` (`NoUID = 0xffffffff`) |
| `SizedString` | 2+n | `u16 len` + bytes |
| `StringArray` | 2+Σ | `u16 count` + count × SizedString |
| `FdArray` | 2+8n | `u16 count` + count × u64 |
| `EmptyMessage` | 0 | (Mount/Channel requests) |
| `ErrorResp` | 4 | `u32 errno` @0 |
| `StatxTimestamp` | 16 | `i64 Sec` @0, `u32 Nsec` @8, `pad i32` @12 |
| `Statx` | 144 | `u32 Mask`@0, `u32 Blksize`@4, `u64 Attributes`@8, `u32 Nlink`@16, `u32 UID`@20, `u32 GID`@24, `u16 Mode`@28, `pad u16`@30, `u64 Ino`@32, `u64 Size`@40, `u64 Blocks`@48, `u64 AttributesMask`@56, `StatxTimestamp Atime`@64, `Btime`@80, `Ctime`@96, `Mtime`@112, `u32 RdevMajor`@128, `u32 RdevMinor`@132, `u32 DevMajor`@136, `u32 DevMinor`@140 |
| `Inode` | 152 | `u64 ControlFD` @0, `Statx` @8 |
| `ChannelResp` | 16 | `i64 dataOffset` @0, `u64 dataLength` @8 |
| `createCommon` | 24 | `u64 DirFD`@0, `u32 UID`@8, `u32 GID`@12, `u16 Mode`@16, `pad u16`@18, `pad u32`@20 |

### Per-message layouts

| MID | Message | Request → Response |
|----:|---------|--------------------|
| 1 | Mount | `EmptyMessage` → `Inode Root` + `u32 MaxMessageSize` + `u16 numSupported` + `n×u16 SupportedMs` (164 B golden) |
| 2 | Channel | `EmptyMessage` → `ChannelResp` + 2 donated FDs (window memfd, fdchannel socket) |
| 3 | FStat | `StatReq`: `u64 FD` (8 B) → `Statx` (144 B) |
| 4 | SetStat | `SetStatReq` (64 B): `u64 FD`@0, `u32 Mask`@8, `u32 Mode`@12, `u32 UID`@16, `u32 GID`@20, `u64 Size`@24, `Timespec Atime`@32, `Mtime`@48 → `SetStatResp` (8 B): `u32 FailureMask`@0, `u32 FailureErrNo`@4 |
| 5 | Walk | `WalkReq`: `u64 DirFD` + `dyn StringArray Path` → `WalkResp`: `i32 Status` + `u16 count` + `count × Inode` |
| 6 | WalkStat | same request → `u16 count` + `count × Statx` |
| 7 | OpenAt | `OpenAtReq` (16 B): `u64 FD`@0, `u32 Flags`@8, `pad u32`@12 → `OpenAtResp`: `u64 OpenFD` (8 B) |
| 8 | OpenCreateAt | `createCommon` + `dyn SizedString Name` → `OpenCreateAtResp` (160 B): `Inode Child`@0, `u64 NewFD`@152 |
| 9 | Close | `dyn FdArray FDs` → `EmptyMessage` |
| 10 | FSync | `StatReq`-shaped `u64 FD` → `EmptyMessage` |
| 11 | PWrite | `u64 Offset` + `u64 FD` + `u32 NumBytes` + `dyn NumBytes bytes` (no trailing pad) → `u64 Count` (8 B) |
| 12 | PRead | `PReadReq` (24 B): `u64 Offset`@0, `u64 FD`@8, `u32 Count`@16, `pad u32`@20 → `u64 NumBytes` + `dyn NumBytes bytes` |
| 13 | MkdirAt | `createCommon` + `dyn SizedString` → `Inode ChildDir` |
| 14 | MknodAt | `createCommon` + `SizedString` → `Inode Child` |
| 15 | SymlinkAt | `u64 DirFD` + `u32 UID`+`u32 GID` + `dyn SizedString Name` + `dyn SizedString Target` → `Inode Symlink` |
| 16 | LinkAt | `u64 DirFD` + `u64 Target` + `dyn SizedString Name` → `Inode Link` |
| 17 | FStatFS | `u64 FD` → `StatFS` (64 B): 8 × u64 fields |
| 18 | FAllocate | `FAllocateReq` (32 B): `u64 FD`@0, `u64 Mode`@8, `u64 Offset`@16, `u64 Length`@24 → `EmptyMessage` |
| 19 | ReadLinkAt | `ReadLinkAtReq`: `u64 FD` (8 B) → `dyn SizedString` (target path) |
| 20 | Flush | `u64 FD` → `EmptyMessage` |
| 21 | Connect | `ConnectReq` (16 B): `u64 FD`@0, `u32 SockType`@8, `pad u32`@12 → `EmptyMessage` |
| 22 | UnlinkAt | `u64 DirFD` + `u32 Flags` + `dyn SizedString Name` → `EmptyMessage` |
| 23 | RenameAt | `u64 oldDirFD` + `u64 newDirFD` + `dyn SizedString oldName` + `dyn SizedString newName` → `EmptyMessage` |
| 24 | Getdents64 | `Getdents64Req` (16 B): `u64 DirFD`@0, `i32 Count`@8, `pad u32`@12 → `u16 count` + `count × Dirent64` |
| 25 | FGetXattr | `u64 FD` + `u32 BufSize` + `dyn SizedString Name` → `dyn SizedString Value` |
| 26 | FSetXattr | `u64 FD` + `u32 Flags` + `dyn SizedString Name` + `dyn SizedString Value` → `EmptyMessage` |
| 27 | FListXattr | `u64 FD` + `u64 Size` → `dyn StringArray` |
| 28 | FRemoveXattr | `u64 FD` + `dyn SizedString Name` → `EmptyMessage` |
| 29 | BindAt | `u64 DirFD` + `u32 SockType` + `dyn SizedString Name` → `Inode Child` + `u64 BoundSocketFD` |
| 30 | Listen | `ListenReq` (16 B): `u64 FD`@0, `i32 Backlog`@8, `pad u32`@12 → `EmptyMessage` |
| 31 | Accept | `u64 FD` → `u64 NewFD` (+ donated socket FD) |
| 32 | ConnectWithCreds | `ConnectReq` + `u32 UID` + `u32 GID` (24 B) → `EmptyMessage` |
| 33 | RenameAt2 | `RenameAtReq` + `u32 Flags` → `EmptyMessage` |

Dirent64 entries use the `struct linux_dirent64` layout (`u64 ino`, `s64 off`,
`u16 reclen`, `u8 type`, name, NUL, padding to 8).

## Golden corpus

`testdata/golden/*.bin` are real `MarshalBytes` outputs for fixed values
(documented in `abi_conformance_test.go`). Third-party implementers should
diff their encoders against these vectors; Oca's differential client uses the
same technique.
