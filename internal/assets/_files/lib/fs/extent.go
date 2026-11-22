package fs

import (
	"unsafe"

	"gokm/kernel"
)

// Extent maps Blocks filesystem blocks from FileBlock to DeviceBlock on the
// mounted device. Units are Device.BlockSize(), not bytes or sectors.
// Extents must be sorted by FileBlock, nonempty, nonoverlapping, and in bounds.
// Gaps are sparse holes. Bytes after EOF in the last mapped block must be zero.
type Extent struct {
	FileBlock   uint64
	DeviceBlock uint64
	Blocks      uint64
}

// ReadMapping is a snapshot of a file's native read mapping. Until Sealed is
// true, ordinary Reader/Writer callbacks remain active. Once true, contents,
// size and mappings must never change, even through other hard links.
type ReadMapping struct {
	Sealed  bool
	Extents []Extent
}

// ExtentReader opts a regular file into native cached reads and readahead.
// ReadMapping is called when instantiating the inode and after successful
// fsync. Return Sealed=false during construction; after persisting payload,
// FileSyncer may seal the node so fsync installs its immutable mapping. It must
// also reject Writer/Truncater mutations as soon as it seals the node.
//
// Extents are copied into kernel-owned memory; do not mutate the returned
// slice. Return no extents until Sealed=true. ReadAt must agree with them.
// Keep all mapped blocks allocated until final Forget AND any additional
// filesystem recovery-generation references are gone. Block-device mounts
// only. Shared writable mmap (including later mprotect) is unsupported for
// these nodes even before sealing. MAP_PRIVATE writes remain supported.
// This interface does not implement allocation, publication or crash recovery.
type ExtentReader interface {
	ReadMapping() (ReadMapping, error)
}

//export gokm_fs_install_mapping
func cInstallMapping(inode uintptr, extents *Extent, count int, sealed bool) int

//export gokm_fs_go_mapping
func goMapping(h uintptr, ino uint64, inode uintptr) (rc int) {
	rc = -int(kernel.EIO)
	defer kernel.CatchPanic[int](nil)
	n, ok := getMount(h).node(ino)
	if !ok {
		return -int(kernel.ENOENT)
	}
	r, ok := n.(ExtentReader)
	if !ok {
		return 0
	}
	mapping, err := r.ReadMapping()
	if err != nil {
		return kernel.ToErrno(err)
	}
	return cInstallMapping(inode, unsafe.SliceData(mapping.Extents), len(mapping.Extents), mapping.Sealed)
}

// WriteFlags select persistence for direct device writes.
type WriteFlags uint32

const (
	// WritePreflush flushes earlier completed writes before submitting this
	// write. It does not wait for unrelated in-flight writes: join them first.
	WritePreflush WriteFlags = 1 << iota
	// WriteFUA makes the written data durable before returning (the block
	// layer emulates this using flushes when native FUA isn't available).
	WriteFUA
)

//export gokm_fs_dev_io
func cDevIO(sb uintptr, block uint64, p *byte, size int, write bool, flags uint32) int

// ReadDirect reads blocks without the device buffer cache, in batched bios.
// Buffers need not be page aligned. Calls wait for completion while other Go
// tasks run. Do not overlap direct I/O with buffer-cache I/O to the same blocks,
// or write blocks backing a live sealed mapping. The filesystem owns this
// coherence rule and must enforce it through allocation and generation lifetimes.
func (d *Device) ReadDirect(block uint64, p []byte) error {
	return d.directIO(block, p, false, 0)
}

// WriteDirect writes blocks without the device buffer cache and waits for
// completion. With flags=0 completion is NOT persistence. To commit: wait for
// all data writes, then write a commit record with WritePreflush|WriteFUA.
// Buffered writes need Writeback/Sync before this protocol. A failed call may
// have partially written data; the filesystem must not publish that generation.
func (d *Device) WriteDirect(block uint64, p []byte, flags WriteFlags) error {
	return d.directIO(block, p, true, flags)
}

func (d *Device) directIO(block uint64, p []byte, write bool, flags WriteFlags) error {
	if flags & ^(WritePreflush|WriteFUA) != 0 {
		return kernel.EINVAL
	}
	if write && d.readOnly {
		return kernel.EROFS
	}
	n := len(p) / d.blockSize
	if n == 0 || len(p)%d.blockSize != 0 || block > d.Blocks() || uint64(n) > d.Blocks()-block {
		return kernel.EINVAL
	}
	if rc := cDevIO(d.sb, block, unsafe.SliceData(p), len(p), write, uint32(flags)); rc != 0 {
		return kernel.Errno(-rc)
	}
	return nil
}
