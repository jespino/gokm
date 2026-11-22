// fsblock tests the block-device filesystem framework. Mounting overwrites a
// disposable device; use ONLY inside the QEMU fixture with its fresh RAM disk.
package main

import (
	"sync"

	"gokm/fs"
	"gokm/kernel"
)

const blockSize = 4096

type root struct {
	dev   *fs.Device
	files map[string]*file
}

func (r *root) InodeID() uint64 { return 1 }
func (r *root) Attr() fs.Attr {
	return fs.Attr{Mode: fs.ModeDir | 0700, Owner: &fs.Owner{UID: 123, GID: 456}}
}
func (r *root) Lookup(name string) (fs.Node, error) {
	f := r.files[name]
	if f == nil {
		return nil, kernel.ENOENT
	}
	return f, nil
}
func (r *root) ReadDir(yield func(string, fs.Node) bool) error {
	for _, name := range []string{"bad-device", "bad-overlap", "bad-zero", "growing", "sealed", "sync-fail"} {
		if !yield(name, r.files[name]) {
			break
		}
	}
	return nil
}
func (r *root) Fsync(int64, int64, bool) error { return r.dev.Sync() }
func (r *root) Sync() error                    { return r.dev.Sync() }

type file struct {
	mu       sync.Mutex
	dev      *fs.Device
	id       uint64
	size     int64
	data     []byte
	sealed   bool
	extents  []fs.Extent
	failSync bool
}

func (f *file) InodeID() uint64 { return f.id }
func (f *file) Attr() fs.Attr {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fs.Attr{Mode: fs.ModeRegular | 0600, Size: f.size, Nlink: 1, Generation: 9}
}
func (f *file) ReadMapping() (fs.ReadMapping, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fs.ReadMapping{Sealed: f.sealed, Extents: f.extents}, nil
}
func (f *file) ReadAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Native reads must bypass this error, including mmap faults/readahead.
	if f.sealed {
		return 0, kernel.EIO
	}
	if off >= int64(len(f.data)) {
		return 0, nil
	}
	return copy(p, f.data[off:]), nil
}
func (f *file) WriteAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sealed {
		return 0, kernel.EPERM
	}
	if off < 0 || off+int64(len(p)) > int64(len(f.data)) {
		return 0, kernel.ENOSPC
	}
	return copy(f.data[off:], p), nil
}
func (f *file) Truncate(n int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sealed {
		return kernel.EPERM
	}
	if n != f.size {
		return kernel.EINVAL
	}
	return nil
}
func (f *file) Fsync(int64, int64, bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSync {
		return kernel.EIO
	}
	if f.sealed {
		return nil
	}
	if err := f.dev.WriteDirect(1024, f.data, fs.WritePreflush|fs.WriteFUA); err != nil {
		return err
	}
	f.extents = []fs.Extent{{FileBlock: 0, DeviceBlock: 1024, Blocks: uint64(len(f.data) / blockSize)}}
	f.sealed = true
	return nil
}

func mount(dev *fs.Device, opts fs.MountOptions) (fs.Dir, error) {
	if opts.ReadOnly || opts.Options != "fixture=overwrite-disposable-device" {
		return nil, kernel.EINVAL
	}
	if err := dev.SetBlockSize(blockSize); err != nil {
		return nil, err
	}
	info := dev.Info()
	if info.LogicalBlockSize == 0 || info.PhysicalBlockSize < info.LogicalBlockSize {
		return nil, kernel.EIO
	}
	if dev.Blocks() < 2048 {
		return nil, kernel.ENOSPC
	}
	// Exercise several bios, unaligned Go memory, read completion and flags.
	buf, err := kernel.MakeBytes((2 << 20) + 1)
	if err != nil {
		return nil, err
	}
	buf = buf[1:]
	for i := range buf {
		buf[i] = byte(i*31 + (i/blockSize)*17 + 7)
	}
	if err := dev.WriteDirect(8, buf, fs.WritePreflush|fs.WriteFUA); err != nil {
		return nil, err
	}
	got, err := kernel.MakeBytes(len(buf))
	if err != nil {
		return nil, err
	}
	if err := dev.ReadDirect(8, got); err != nil {
		return nil, err
	}
	for i := range buf {
		if got[i] != buf[i] {
			return nil, kernel.EIO
		}
	}
	if err := dev.WriteBlocks(1500, buf[:blockSize]); err != nil {
		return nil, err
	}
	if err := dev.Sync(); err != nil {
		return nil, err
	}
	if err := dev.ReadDirect(1500, got[:blockSize]); err != nil {
		return nil, err
	}
	for i := 0; i < blockSize; i++ {
		if got[i] != buf[i] {
			return nil, kernel.EIO
		}
	}
	if dev.ReadDirect(dev.Blocks(), got[:blockSize]) != kernel.EINVAL {
		return nil, kernel.EIO
	}
	// A sparse two-run mapping, with a short final page and zeroed tail.
	const run = 128 * blockSize
	for i := 0; i < run; i++ {
		buf[i] = byte(i*31 + (i/blockSize)*17 + 7)
	}
	if err := dev.WriteDirect(8, buf[:run], 0); err != nil {
		return nil, err
	}
	for i := 0; i < run; i++ {
		buf[i] = byte((i+129*blockSize)*31 + ((i+129*blockSize)/blockSize)*17 + 7)
	}
	clear(buf[run-37 : run])
	if err := dev.WriteDirect(256, buf[:run], fs.WritePreflush|fs.WriteFUA); err != nil {
		return nil, err
	}
	r := &root{dev: dev, files: map[string]*file{}}
	r.files["sealed"] = &file{dev: dev, id: 2, size: 257*blockSize - 37, sealed: true, extents: []fs.Extent{
		{FileBlock: 0, DeviceBlock: 8, Blocks: 128}, {FileBlock: 129, DeviceBlock: 256, Blocks: 128},
	}}
	r.files["growing"] = &file{dev: dev, id: 3, size: 3 * blockSize, data: make([]byte, 3*blockSize)}
	r.files["sync-fail"] = &file{dev: dev, id: 4, size: blockSize, data: make([]byte, blockSize), failSync: true}
	r.files["bad-zero"] = &file{dev: dev, id: 5, size: blockSize, sealed: true, extents: []fs.Extent{{Blocks: 0}}}
	r.files["bad-device"] = &file{dev: dev, id: 6, size: blockSize, sealed: true, extents: []fs.Extent{{DeviceBlock: dev.Blocks(), Blocks: 1}}}
	r.files["bad-overlap"] = &file{dev: dev, id: 7, size: 2 * blockSize, sealed: true, extents: []fs.Extent{{Blocks: 2}, {FileBlock: 1, Blocks: 1}}}
	return r, nil
}
func main() {
	if err := fs.Register(&fs.FileSystem{Name: "fsblock", MountDevice: mount}); err != nil {
		kernel.FailLoad(err)
	}
}
