package fs

import (
	"time"

	"gokm/kernel"
)

// Block-device filesystems: set FileSystem.MountDevice instead of Mount and
// the filesystem is mounted from a block device
// (mount -t name /dev/sdb1 /mnt). MountDevice gets the device to read and
// write blocks through the kernel's buffer cache.

// Device is the block device a filesystem is mounted from.
type Device struct {
	sb        uintptr // struct super_block
	blockSize int
	size      int64 // bytes
	readOnly  bool
}

// MountOptions describes a mount.
type MountOptions struct {
	ReadOnly bool
	// Options contains comma-separated filesystem-specific mount options.
	// VFS options (ro, noexec, etc.) are handled by Linux. Implementations
	// must reject unknown options instead of silently ignoring them.
	Options string
}

// Implemented in the fs glue (gokm/frameworks/fs/fs.c).
//
//export gokm_fs_dev_read
func cDevRead(sb uintptr, block uint64, dst *byte, nblocks int) int

//export gokm_fs_dev_write
func cDevWrite(sb uintptr, block uint64, src *byte, nblocks int) int

//export gokm_fs_dev_sync
func cDevSync(sb uintptr, flush bool) int

//export gokm_fs_dev_set_blocksize
func cDevSetBlockSize(sb uintptr, size int) int

//export gokm_fs_dev_info
func cDevInfo(sb uintptr, logical, physical *uint32, cache, fua *bool)

// DeviceInfo describes the queue properties reported by the block layer.
// WriteCache=false means no volatile write cache is advertised, not that
// hardware has been independently verified. Linux handles FUA emulation.
type DeviceInfo struct {
	LogicalBlockSize, PhysicalBlockSize uint32
	WriteCache, FUA                     bool
}

func (d *Device) Info() DeviceInfo {
	var info DeviceInfo
	cDevInfo(d.sb, &info.LogicalBlockSize, &info.PhysicalBlockSize, &info.WriteCache, &info.FUA)
	return info
}

// BlockSize returns the block size used by ReadBlocks/WriteBlocks (512 at
// mount time).
func (d *Device) BlockSize() int { return d.blockSize }

// Size returns the device size in bytes.
func (d *Device) Size() int64 { return d.size }

// Blocks returns the device size in blocks.
func (d *Device) Blocks() uint64 { return uint64(d.size) / uint64(d.blockSize) }

// ReadOnly reports whether the filesystem is mounted read-only.
func (d *Device) ReadOnly() bool { return d.readOnly }

// SetBlockSize changes the block size (a power of two between the device's
// logical block size and the page size). Call only during mount, before any
// I/O or inode creation. It must not race with device operations.
func (d *Device) SetBlockSize(size int) error {
	if size <= 0 || size&(size-1) != 0 {
		return kernel.EINVAL
	}
	if rc := cDevSetBlockSize(d.sb, size); rc != 0 {
		return kernel.Errno(-rc)
	}
	d.blockSize = size
	return nil
}

// ReadBlocks reads len(p)/BlockSize() blocks starting at block into p
// (len(p) must be a multiple of the block size). Reads go through the
// kernel's buffer cache; other Go code keeps running while this one waits
// for the device.
func (d *Device) ReadBlocks(block uint64, p []byte) error {
	n := len(p) / d.blockSize
	if n == 0 || len(p)%d.blockSize != 0 || block > d.Blocks() || uint64(n) > d.Blocks()-block {
		return kernel.EINVAL
	}
	if rc := cDevRead(d.sb, block, &p[0], n); rc != 0 {
		return kernel.Errno(-rc)
	}
	return nil
}

// WriteBlocks writes p (a multiple of the block size) starting at block.
// Data reaches the buffer cache; it's written to the device later, or on
// Sync.
func (d *Device) WriteBlocks(block uint64, p []byte) error {
	n := len(p) / d.blockSize
	if n == 0 || len(p)%d.blockSize != 0 || block > d.Blocks() || uint64(n) > d.Blocks()-block {
		return kernel.EINVAL
	}
	if d.readOnly {
		return kernel.EROFS
	}
	if rc := cDevWrite(d.sb, block, &p[0], n); rc != 0 {
		return kernel.Errno(-rc)
	}
	return nil
}

// Sync writes dirty buffer-cache blocks, waits, and flushes the device's
// volatile write cache. A successful return is a persistence barrier for
// preceding buffered writes. It does not construct filesystem transactions.
func (d *Device) Sync() error {
	if rc := cDevSync(d.sb, true); rc != 0 {
		return kernel.Errno(-rc)
	}
	return nil
}

// Writeback waits for dirty buffer-cache blocks without flushing the device's
// volatile cache. This is NOT a durability barrier. Use Sync for persistence.
func (d *Device) Writeback() error {
	if rc := cDevSync(d.sb, false); rc != 0 {
		return kernel.Errno(-rc)
	}
	return nil
}

// More optional interfaces.
type (
	// Statfser is implemented by a root Dir to report statfs(2) numbers
	// (otherwise they're zero).
	Statfser interface {
		Statfs() (StatFS, error)
	}
	// Syncer is implemented by a root Dir that caches metadata: Sync
	// writes it to the device (sync(2), syncfs(2), fsync(2), unmount).
	Syncer interface {
		Sync() error
	}
	// SetTimer is implemented by nodes whose timestamps can be changed
	// (touch, utimes). A nil time means unchanged.
	SetTimer interface {
		SetTimes(atime, mtime *time.Time) error
	}
	// Forgetter is implemented by nodes that want to know when the kernel
	// is done with them after they were removed (e.g. the last close of an
	// unlinked file): the moment to release their storage.
	Forgetter interface {
		Forget()
	}
)

// StatFS are filesystem statistics, in units of BlockSize.
type StatFS struct {
	BlockSize   int64
	Blocks      uint64
	BlocksFree  uint64
	BlocksAvail uint64
	Files       uint64
	FilesFree   uint64
	NameLen     int64
}

func nsOf(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func timeOf(ns int64) *time.Time {
	if ns == 0 {
		return nil
	}
	t := time.Unix(0, ns)
	return &t
}

// Called by the glue between GOKM_ENTER and GOKM_LEAVE.

//export gokm_fs_go_statfs
func goStatfs(h uintptr, bsize, blocks, bfree, bavail, files, ffree, namelen *int64) (rc int) {
	rc = -int(kernel.EIO)
	defer kernel.CatchPanic[int](nil)
	s, ok := getMount(h).root.(Statfser)
	if !ok {
		return -int(kernel.ENOSYS)
	}
	st, err := s.Statfs()
	if err != nil {
		return kernel.ToErrno(err)
	}
	*bsize, *blocks, *bfree, *bavail = st.BlockSize, int64(st.Blocks), int64(st.BlocksFree), int64(st.BlocksAvail)
	*files, *ffree, *namelen = int64(st.Files), int64(st.FilesFree), st.NameLen
	return 0
}

//export gokm_fs_go_sync
func goSync(h uintptr) (rc int) {
	rc = -int(kernel.EIO)
	defer kernel.CatchPanic[int](nil)
	if s, ok := getMount(h).root.(Syncer); ok {
		return kernel.ToErrno(s.Sync())
	}
	return 0
}

//export gokm_fs_go_fsync
func goFsync(h uintptr, ino uint64, start, end int64, datasync bool, handled *bool) (rc int) {
	rc = -int(kernel.EIO)
	defer kernel.CatchPanic[int](nil)
	*handled = false
	n, ok := getMount(h).node(ino)
	if !ok {
		return -int(kernel.ENOENT)
	}
	if s, ok := n.(FileSyncer); ok {
		*handled = true
		return kernel.ToErrno(s.Fsync(start, end, datasync))
	}
	return goSync(h)
}

//export gokm_fs_go_settimes
func goSetTimes(h uintptr, ino uint64, atime, mtime int64) (rc int) {
	rc = -int(kernel.EIO)
	defer kernel.CatchPanic[int](nil)
	n, ok := getMount(h).node(ino)
	if !ok {
		return -int(kernel.ENOENT)
	}
	st, ok := n.(SetTimer)
	if !ok {
		return 0 // kept in the in-memory inode only
	}
	return kernel.ToErrno(st.SetTimes(timeOf(atime), timeOf(mtime)))
}
