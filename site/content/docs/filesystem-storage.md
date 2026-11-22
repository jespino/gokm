---
title: "Persistent filesystem building blocks"
description: "Durability, hard links, inode lifetime and native immutable extent reads."
weight: 12
---

`gokm/fs` provides the VFS and block I/O mechanisms needed to build a local
persistent filesystem. The filesystem still implements its own disk format,
allocation, journaling, recovery, reference accounting and publication protocol.

## Sync and persistence

Files and directories can implement:

```go
type FileSyncer interface {
    Fsync(start, end int64, datasync bool) error
}
```

Offsets are inclusive. Linux writes back the file's dirty mapped pages before
calling this method. Directory fsync invokes the directory's method too.
`FileSyncer` owns the complete persistence protocol: persist the captured
generation and all dependencies, including required device-cache flushes, before
returning success. It may batch concurrent waiters, but cannot wait for a future
application operation that is itself waiting for this fsync to return.

No whole-device sync follows a handled `FileSyncer` call. This lets a filesystem
avoid flushing an unrelated active merge. Nodes without `FileSyncer` retain the
legacy root `Syncer` fallback, followed by buffer-cache writeback and a device
cache flush. Errors propagate to fsync. The framework also honors synchronous
callback writes through `generic_write_sync`, including `O_SYNC`/`O_DSYNC`.

Device operations distinguish completion from persistence:

| Method | Meaning |
| --- | --- |
| `WriteBlocks` | Dirty the device buffer cache |
| `Writeback` | Wait for dirty buffer-cache writes; volatile device cache may remain |
| `Sync` | Write back and flush the device cache |
| `ReadDirect` | Read through batched bios without the device buffer cache |
| `WriteDirect(..., 0)` | Complete direct bios; no durability guarantee |
| `WriteDirect(..., WritePreflush\|WriteFUA)` | Flush earlier completed device writes before this write, then persist its contents before returning |

For a data-before-commit protocol, wait for every data write first, then issue
the commit-record write with preflush and FUA. Buffered data also needs writeback
before issuing that direct commit. A preflush does not join unrelated in-flight
I/O, serialize filesystem transactions, or flush dirty Go metadata.

Direct calls can run concurrently from Go tasks. They wait for completion with
Go execution parked; no completion interrupt calls Go. The buffer must remain
exclusively owned by the caller until return. A failed call can have partially
written data. Calls validate lengths, device bounds, write flags and read-only
state. `Device.Info()` exposes logical/physical block sizes and advertised cache/
FUA properties; the block layer handles native FUA versus flush emulation.

Do not mix cached and direct accesses to changing versions of the same device
blocks. Separate metadata and payload regions or explicitly manage coherence.
Never overwrite blocks backing a live sealed file. Mount-time `SetBlockSize`
must precede I/O and inode creation, and must not race with operations.

## Identity, links and lifetime

`Identifier.InodeID()` supplies persistent IDs: root is 1, others are positive
signed-64-bit values above 1. Return the same node object for every live ID,
including hard-link aliases. A second live object with an occupied ID is rejected.
Supply `Attr.Generation` when reusing an ID after all prior references are gone.

`Linker.Link(name, target)` adds a regular-file hard link to a directory. The
implementation must retain the exact node and atomically update persistent
link counts. Its `Remove` and replacing `Rename` must decrement those counts.
Report the actual persistent count in `Attr.Nlink` so inode reconstruction after
cache eviction restores it. The framework updates VFS link counts on successful
operations. Renaming two names of the same inode is a no-op. Directories cannot
be hard-linked.

Lifecycle hooks have distinct meanings:

- `Opener.Open(flags)` and `Releaser.Release(flags)` track an open file
  description, including the references held by VMAs; they do not count each dup.
- `Evicter.Evict()` releases cached node state on any final kernel inode eviction,
  including an inode still linked on disk.
- `Forgetter.Forget()` runs only when an unlinked inode is finally evicted.
  The filesystem must additionally preserve blocks reachable by recovery
  generations before reusing their storage.

After eviction, persistent-ID nodes leave the framework's cache and may be
reconstructed by `Lookup`. Legacy pointer-identified linked nodes keep their
existing identity behavior. Persistent directory listing reports IDs without
retaining every node. Large directories can implement `DirReader.ReadDirFrom`
with stable increasing cookies instead of rescanning from the beginning on
each VFS read. `Dir.ReadDir` is still required for emptiness checks.

`Attr.Owner` optionally initializes fixed UID/GID ownership in the initial user
namespace. Filesystem-specific options reach `MountDevice` or `MountWithOptions`
as `MountOptions.Options`; validate and reject unknown options in the callback.
The legacy `Mount` constructor rejects custom options. Commas and embedded NUL
values are not supported in the textual option transport; total size is bounded
to one page. Standard VFS options remain handled by Linux.

## Native sealed extent reads

For immutable payloads, implement `ExtentReader` on a regular file:

```go
func (f *partFile) ReadMapping() (fs.ReadMapping, error) {
    return fs.ReadMapping{
        Sealed: true,
        Extents: []fs.Extent{
            {FileBlock: 0, DeviceBlock: 8192, Blocks: 256},
            {FileBlock: 256, DeviceBlock: 16384, Blocks: 128},
        },
    }, nil
}
```

This is for block-device mounts. Extent units are the configured filesystem
block size. Runs must be sorted, nonempty, nonoverlapping in file space and
within file/device bounds. Gaps read as zero. The final physical block must have
a zeroed EOF tail. Never mutate a returned extent slice. The framework copies
descriptors into kernel-owned memory; no kernel object keeps a Go pointer.

The inode's mapping is installed on instantiation or after successful fsync.
For construction, return `Sealed: false` with no extents and use ordinary
Reader/Writer callbacks. Once data is durable, your `FileSyncer` can permanently
seal the node; it must immediately reject Writer/Truncater mutations itself.
After that fsync succeeds, the framework installs the mapping and also rejects
syscall writes/truncate. An installed sealed map never changes.

Native reads use `generic_file_read_iter`, `mpage_read_folio` and
`mpage_readahead`. Cached pread and mmap share the file page cache; misses look up
kernel extent descriptors and submit bios without Go payload callbacks or a
second device buffer-cache copy. The mapping is freed at inode eviction. DONTNEED
advice and mincore operate on the same file cache.

For any `ExtentReader`, shared writable mmap is rejected even during construction.
Read-only shared mappings have shared-write permission removed so later
`mprotect` cannot race sealing. Private writable mappings remain available.
Native `O_DIRECT` file reads are unsupported; direct **device** I/O is a separate
API. ReadAt callbacks must describe the same contents while they are in use.

Mutable file writes still use the Go callback path; this is not a general mutable
iomap backend. `FileSystem.MaxIOSize` allows callback buffers from 4 KiB to 16 MiB
instead of the default 128 KiB, reducing syscall splitting for streaming writes.
Applications do not need to opt into sealed mappings for the other features.

## Tests

`make test` includes race-enabled host execution of the real Go callbacks with
only foreign/runtime boundaries substituted. `make qemu` includes hard-link,
rename, directory sync and flock tests in gofs, and the `fsblock` fixture for
direct bios, persistence flags, sparse native reads, mmap protection, sealing,
error propagation and invalid extent descriptors.

`fsblock` deliberately overwrites a fresh RAM disk in its disposable guest and
requires an explicit fixture mount option. It is not a persistent filesystem
example or a crash-consistency proof. Successful compilation alone does not
establish device durability or runtime correctness; run the VM tests before
using these interfaces for real storage.
