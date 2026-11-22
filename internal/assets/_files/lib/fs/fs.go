// Package fs lets Go code implement a Linux filesystem.
//
// Implement Dir and file nodes, register a FileSystem, and it can be mounted
// with `mount -t <name> none /mnt`. gokm compiles the kernel-side glue (VFS
// callbacks, inode/dentry caches, user-memory copies) into the module
// automatically when this package is imported.
//
//	type root struct{ ... }   // implements fs.Dir (+ Creator, Mkdirer, Remover)
//	type file struct{ ... }   // implements fs.Node (+ Reader, Writer, Truncater)
//
//	func main() {
//		err := fs.Register(&fs.FileSystem{
//			Name:  "myfs",
//			Mount: func() (fs.Dir, error) { return newRoot(), nil },
//		})
//		if err != nil {
//			kernel.FailLoad(err)
//		}
//	}
//
// Nodes must be comparable (use pointer types): the framework maps each node
// to an inode number. Methods return kernel.Errno values (kernel.ENOENT,
// kernel.ENOSPC, ...) to report specific errors; other errors become EIO.
// Methods can be called concurrently from several CPUs (like HTTP handlers):
// protect shared state with a sync.Mutex. The kernel already serializes
// some operations (creating/removing entries in one directory, writes to
// one file).
package fs

import (
	"io"
	"sync"
	"time"
	"unsafe"

	"gokm/kernel"
)

// Mode is a file type and permission bits, with Linux's layout (S_IFDIR...).
type Mode uint32

const (
	ModeType    Mode = 0o170000
	ModeDir     Mode = 0o040000
	ModeRegular Mode = 0o100000
	ModeSymlink Mode = 0o120000
	ModePerm    Mode = 0o7777
)

func (m Mode) IsDir() bool     { return m&ModeType == ModeDir }
func (m Mode) IsRegular() bool { return m&ModeType == ModeRegular }
func (m Mode) IsSymlink() bool { return m&ModeType == ModeSymlink }

// Attr are a node's attributes.
type Attr struct {
	Mode Mode  // type and permissions
	Size int64 // size in bytes (regular files)
	// Nlink, when nonzero, initializes the inode's persistent link count.
	// Filesystems supporting hard links must report the actual count here.
	Nlink uint32
	// Generation distinguishes reused persistent inode numbers.
	Generation uint32
	// Owner, when set, supplies ownership in the initial user namespace.
	// A nil Owner preserves the framework's default/VFS-assigned ownership.
	Owner *Owner
	// Timestamps; zero values leave the kernel's in-memory ones.
	Atime, Mtime, Ctime time.Time
}

type Owner struct{ UID, GID uint32 }

// Node is anything in the filesystem.
type Node interface {
	Attr() Attr
}

// Dir is a directory.
type Dir interface {
	Node
	// Lookup returns the child called name, or (nil, nil) / kernel.ENOENT
	// if there is none.
	Lookup(name string) (Node, error)
	// ReadDir calls yield for each entry, in a stable order, until yield
	// returns false.
	ReadDir(yield func(name string, n Node) bool) error
}

// Optional interfaces. Operations a node doesn't implement fail with EPERM
// (directories) or EINVAL / EBADF (files).
type (
	// DirReader resumes iteration without scanning an entire directory.
	// Cookie 0 means the beginning. Yield cookies must strictly increase,
	// fit in [1, 2^63-3], and remain valid across calls (deleted entries may
	// leave gaps). Stop when yield returns false. Dir.ReadDir remains needed
	// for emptiness checks; this interface handles resumable VFS iteration.
	DirReader interface {
		ReadDirFrom(cookie uint64, yield func(next uint64, name string, n Node) bool) error
	}
	// Identifier supplies a persistent inode number. Root must use 1; other
	// IDs must be in [2, 2^63-1]. Return the same Node for a live ID, including
	// hard-link aliases. Never reuse an ID while the old node is live.
	Identifier interface {
		InodeID() uint64
	}
	// Linker adds a regular-file hard link. It must retain the exact target
	// node, atomically increment its persistent link count, and return EEXIST
	// if name exists. Remove and replacement Rename must decrement counts.
	Linker interface {
		Link(name string, target Node) error
	}
	// FileSyncer implements fsync/fdatasync on a file or directory. Start and
	// end are inclusive byte offsets (directory calls cover the whole node).
	// It must durably commit the captured generation and its dependencies,
	// including device cache flushes. No mount-wide Sync/device sync follows
	// this callback. Without it, the framework uses the legacy root Syncer
	// followed by a device sync and cache flush.
	FileSyncer interface {
		Fsync(start, end int64, datasync bool) error
	}
	// Opener and Releaser observe an open file description, not each dup or
	// VMA. Release runs after the last descriptor/VMA reference is gone. Flags
	// use Linux O_* values. Open may reject an operation; Release cannot fail.
	Opener interface {
		Open(flags uint32) error
	}
	Releaser interface {
		Release(flags uint32)
	}
	// Evicter releases cached node state when a kernel inode is evicted,
	// including linked persistent nodes. It must not free linked storage.
	// Identifier nodes are then removed from the framework's node cache, so
	// future Lookup may reconstruct them. Forgetter is only for final unlink.
	Evicter interface{ Evict() }
	// Creator creates regular files in a directory.
	Creator interface {
		Create(name string, mode Mode) (Node, error)
	}
	// Mkdirer creates subdirectories.
	Mkdirer interface {
		Mkdir(name string, mode Mode) (Dir, error)
	}
	// Remover removes an entry (a file, or an empty directory: the
	// framework checks emptiness before calling it for rmdir).
	Remover interface {
		Remove(name string) error
	}
	// Reader reads file content at off. Return 0 (with nil or io.EOF) at
	// end of file.
	Reader interface {
		ReadAt(p []byte, off int64) (int, error)
	}
	// Writer writes file content at off, growing the file if needed.
	Writer interface {
		WriteAt(p []byte, off int64) (int, error)
	}
	// Truncater changes a file's size (truncate, O_TRUNC).
	Truncater interface {
		Truncate(size int64) error
	}
	// Renamer moves the entry oldName of this directory to newName in
	// newDir (possibly the same directory), replacing what's there. The
	// framework already checked that a replaced directory is empty and that
	// the types match.
	Renamer interface {
		Rename(oldName string, newDir Dir, newName string) error
	}
	// Symlinker creates symbolic links in a directory. The returned node
	// should report ModeSymlink and implement Readlinker.
	Symlinker interface {
		Symlink(name, target string) (Node, error)
	}
	// Readlinker is implemented by symbolic links.
	Readlinker interface {
		Readlink() (string, error)
	}
	// Unmounter is implemented by a root Dir that wants to know when its
	// mount goes away.
	Unmounter interface {
		Unmount()
	}
	// Xattrer stores a node's extended attributes (getfattr/setfattr):
	// names come with their namespace prefix, "user." or "trusted." (the
	// kernel checks permissions; trusted.* needs CAP_SYS_ADMIN). GetXattr
	// and RemoveXattr return kernel.ENODATA for a missing name. SetXattr
	// gets a copy of the value; the framework handles the create-only and
	// replace-only flags. Without Xattrer, setting fails with EOPNOTSUPP.
	Xattrer interface {
		GetXattr(name string) ([]byte, error)
		SetXattr(name string, value []byte) error
		RemoveXattr(name string) error
		ListXattrs() ([]string, error)
	}
)

// FileSystem is a filesystem type.
type FileSystem struct {
	Name string
	// Magic is reported by statfs (f_type). Default: 0x676f6b6d ("gokm").
	Magic uint32
	// Mount is called for each mount and returns its root directory.
	Mount func() (Dir, error)
	// MountWithOptions is the nodev alternative to Mount that receives options.
	MountWithOptions func(opts MountOptions) (Dir, error)
	// MountDevice, instead of Mount, makes this a block-device filesystem
	// (mount -t name /dev/... /mnt): it gets the device and returns the
	// root directory.
	MountDevice func(dev *Device, opts MountOptions) (Dir, error)
	// MaxFileSize limits file sizes (default: the kernel's maximum).
	MaxFileSize int64
	// MaxIOSize bounds one callback read/write buffer. Zero keeps the legacy
	// 128 KiB limit. Values from 4 KiB to 16 MiB allow large streaming writes
	// without splitting every syscall at 128 KiB. Native reads are not capped.
	MaxIOSize int

	ptr    uintptr // kernel struct, 0 when not registered
	handle kernel.Handle
}

// Implemented in the fs glue (gokm/frameworks/fs/fs.c).
//
//export gokm_fs_register
func cRegister(name *byte, nameLen int, magic uint32, requiresDev bool, maxBytes int64, maxIO int, handle uintptr, out *uintptr) int

//export gokm_fs_unregister
func cUnregister(ptr uintptr)

//export gokm_fs_emit
func cEmit(ctx uintptr, name *byte, nameLen int, ino uint64, dtype uint32) bool

//export gokm_fs_emit_cookie
func cEmitCookie(ctx uintptr, name *byte, nameLen int, ino uint64, dtype uint32, next int64) bool

// Register registers the filesystem type with the kernel. It's unregistered
// automatically at module exit (or earlier with Unregister).
func Register(f *FileSystem) error {
	constructors := 0
	if f.Mount != nil {
		constructors++
	}
	if f.MountWithOptions != nil {
		constructors++
	}
	if f.MountDevice != nil {
		constructors++
	}
	if f.Name == "" || constructors != 1 {
		return kernel.EINVAL
	}
	if f.MaxFileSize < 0 || (f.MaxIOSize != 0 && (f.MaxIOSize < 4096 || f.MaxIOSize > 16<<20)) {
		return kernel.EINVAL
	}
	if f.ptr != 0 {
		return kernel.EBUSY
	}
	if f.Magic == 0 {
		f.Magic = 0x676f6b6d
	}
	f.handle = kernel.NewHandle(f)
	name := f.Name + "\x00"
	if rc := cRegister(unsafe.StringData(name), len(f.Name), f.Magic, f.MountDevice != nil, f.MaxFileSize, f.MaxIOSize, uintptr(f.handle), &f.ptr); rc != 0 {
		f.handle.Delete()
		f.ptr = 0
		return kernel.Errno(-rc)
	}
	kernel.OnExit(f.Unregister)
	return nil
}

// Unregister unregisters the filesystem type. Mounted instances keep the
// module loaded, so this only happens once all are unmounted.
func (f *FileSystem) Unregister() {
	if f.ptr == 0 {
		return
	}
	cUnregister(f.ptr)
	f.ptr = 0
	f.handle.Delete()
}

// mount is one mounted instance: it maps nodes to inode numbers.
type mount struct {
	mu    sync.Mutex // protects nodes, inos, next
	root  Dir
	nodes map[uint64]Node
	inos  map[nodeKey]uint64
	next  uint64
}

// nodeKey identifies a node: the two words of its interface value (type and
// pointer). Hashing interface values directly would pull floating-point code
// (for float-typed dynamic values) into the module, which the kernel can't
// link. For pointer node types, this is pointer identity.
type nodeKey struct{ typ, ptr uintptr }

func keyOf(n Node) nodeKey { return *(*nodeKey)(unsafe.Pointer(&n)) }

const rootIno = 1

func (m *mount) ino(n Node) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := keyOf(n)
	if ino, ok := m.inos[k]; ok {
		return int64(ino)
	}
	var ino uint64
	if id, ok := n.(Identifier); ok {
		ino = id.InodeID()
		if ino == 0 || ino > uint64(^uint64(0)>>1) {
			return -int64(kernel.EINVAL)
		}
		if _, exists := m.nodes[ino]; exists {
			return -int64(kernel.EEXIST)
		}
	} else {
		for {
			ino = m.next
			if ino == 0 || ino > uint64(^uint64(0)>>1) {
				return -int64(kernel.EOVERFLOW)
			}
			m.next++
			if _, exists := m.nodes[ino]; !exists {
				break
			}
		}
	}
	m.nodes[ino] = n
	m.inos[k] = ino
	return int64(ino)
}

func (m *mount) forget(ino uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.nodes[ino]; ok {
		delete(m.nodes, ino)
		delete(m.inos, keyOf(n))
	}
}

func getMount(h uintptr) *mount { return kernel.Handle(h).Value().(*mount) }

func (m *mount) node(ino uint64) (Node, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[ino]
	return n, ok
}

func (m *mount) dir(ino uint64) (Dir, int) {
	n, ok := m.node(ino)
	if !ok {
		return nil, -int(kernel.ENOENT)
	}
	d, ok := n.(Dir)
	if !ok {
		return nil, -int(kernel.ENOTDIR)
	}
	return d, 0
}

func (m *mount) entryIno(n Node) int64 {
	if id, ok := n.(Identifier); ok {
		num := id.InodeID()
		if num == 0 || num > uint64(^uint64(0)>>1) {
			return -int64(kernel.EINVAL)
		}
		return int64(num)
	}
	return m.ino(n)
}

// withType fills in the type bits of a node's mode if Attr left them out.
func withType(n Node, mode Mode) Mode {
	if mode&ModeType != 0 {
		return mode
	}
	switch n.(type) {
	case Dir:
		return mode | ModeDir
	case Readlinker:
		return mode | ModeSymlink
	}
	return mode | ModeRegular
}

func viewString(p *byte, n int) string { return unsafe.String(p, n) }

func viewBytes(p *byte, n int) []byte {
	if n == 0 {
		return nil
	}
	return unsafe.Slice(p, n)
}

// The functions below are called by the fs glue, between GOKM_ENTER and
// GOKM_LEAVE. Go int is 64 bits (long in C).

//export gokm_fs_go_mount
func goMount(fsHandle, sb uintptr, blockSize int, size int64, readOnly bool, options *byte, optionsLen int, out *uintptr) (rc int) {
	rc = -int(kernel.EIO)
	defer kernel.CatchPanic[int](nil)
	f := kernel.Handle(fsHandle).Value().(*FileSystem)
	var root Dir
	var err error
	opts := MountOptions{ReadOnly: readOnly, Options: string(viewBytes(options, optionsLen))}
	if f.MountDevice != nil {
		dev := &Device{sb: sb, blockSize: blockSize, size: size, readOnly: readOnly}
		root, err = f.MountDevice(dev, opts)
	} else if f.MountWithOptions != nil {
		root, err = f.MountWithOptions(opts)
	} else {
		if opts.Options != "" {
			return -int(kernel.EINVAL)
		}
		root, err = f.Mount()
	}
	if err != nil {
		return kernel.ToErrno(err)
	}
	if root == nil {
		return -int(kernel.EINVAL)
	}
	m := &mount{root: root, nodes: map[uint64]Node{}, inos: map[nodeKey]uint64{}, next: rootIno}
	if ino := m.ino(root); ino != rootIno {
		if u, ok := root.(Unmounter); ok {
			u.Unmount()
		}
		return -int(kernel.EINVAL)
	}
	*out = uintptr(kernel.NewHandle(m))
	return 0
}

// gokm_fs_go_unmount runs before the device is released (put_super).
//
//export gokm_fs_go_unmount
func goUnmount(h uintptr) {
	defer kernel.CatchPanic[int](nil)
	if u, ok := getMount(h).root.(Unmounter); ok {
		u.Unmount()
	}
}

//export gokm_fs_go_release
func goRelease(h uintptr) {
	defer kernel.CatchPanic[int](nil)
	kernel.Handle(h).Delete()
}

//export gokm_fs_go_stat
func goStat(h uintptr, ino uint64, mode, nlink, generation, uid, gid *uint32, owner *bool, size, atime, mtime, ctime *int64) (rc int) {
	rc = -int(kernel.EIO)
	defer kernel.CatchPanic[int](nil)
	n, ok := getMount(h).node(ino)
	if !ok {
		return -int(kernel.ENOENT)
	}
	a := n.Attr()
	a.Mode = withType(n, a.Mode)
	*mode = uint32(a.Mode)
	*nlink, *generation = a.Nlink, a.Generation
	*owner = a.Owner != nil
	if a.Owner != nil {
		*uid, *gid = a.Owner.UID, a.Owner.GID
	}
	*size = a.Size
	*atime, *mtime, *ctime = nsOf(a.Atime), nsOf(a.Mtime), nsOf(a.Ctime)
	if a.Mode.IsSymlink() && a.Size == 0 {
		if rl, ok := n.(Readlinker); ok {
			if t, err := rl.Readlink(); err == nil {
				*size = int64(len(t))
			}
		}
	}
	return 0
}

// gokm_fs_go_lookup returns the child's inode number, 0 if it doesn't exist,
// or a negative errno.
//
//export gokm_fs_go_lookup
func goLookup(h uintptr, dirIno uint64, name *byte, nameLen int) (rc int64) {
	rc = -int64(kernel.EIO)
	defer kernel.CatchPanic[int64](nil)
	m := getMount(h)
	d, errno := m.dir(dirIno)
	if errno != 0 {
		return int64(errno)
	}
	n, err := d.Lookup(viewString(name, nameLen))
	if err == kernel.ENOENT || (err == nil && n == nil) {
		return 0
	}
	if err != nil {
		return int64(kernel.ToErrno(err))
	}
	return int64(m.ino(n))
}

//export gokm_fs_go_create
func goCreate(h uintptr, dirIno uint64, name *byte, nameLen int, mode uint32) (rc int64) {
	rc = -int64(kernel.EIO)
	defer kernel.CatchPanic[int64](nil)
	m := getMount(h)
	d, errno := m.dir(dirIno)
	if errno != 0 {
		return int64(errno)
	}
	nm := string(viewBytes(name, nameLen)) // copy: the kernel buffer is transient
	var n Node
	var err error
	if Mode(mode).IsDir() {
		mk, ok := d.(Mkdirer)
		if !ok {
			return -int64(kernel.EPERM)
		}
		var nd Dir
		nd, err = mk.Mkdir(nm, Mode(mode))
		if nd != nil {
			n = nd
		}
	} else {
		cr, ok := d.(Creator)
		if !ok {
			return -int64(kernel.EPERM)
		}
		n, err = cr.Create(nm, Mode(mode))
	}
	if err != nil {
		return int64(kernel.ToErrno(err))
	}
	if n == nil {
		return -int64(kernel.EIO)
	}
	return int64(m.ino(n))
}

//export gokm_fs_go_remove
func goRemove(h uintptr, dirIno uint64, name *byte, nameLen int, isDir bool) (rc int) {
	rc = -int(kernel.EIO)
	defer kernel.CatchPanic[int](nil)
	m := getMount(h)
	d, errno := m.dir(dirIno)
	if errno != 0 {
		return errno
	}
	rm, ok := d.(Remover)
	if !ok {
		return -int(kernel.EPERM)
	}
	nm := viewString(name, nameLen)
	if isDir {
		child, err := d.Lookup(nm)
		if err != nil {
			return kernel.ToErrno(err)
		}
		if child == nil {
			return -int(kernel.ENOENT)
		}
		cd, ok := child.(Dir)
		if !ok {
			return -int(kernel.ENOTDIR)
		}
		empty := true
		if err := cd.ReadDir(func(string, Node) bool { empty = false; return false }); err != nil {
			return kernel.ToErrno(err)
		}
		if !empty {
			return -int(kernel.ENOTEMPTY)
		}
	}
	return kernel.ToErrno(rm.Remove(nm))
}

//export gokm_fs_go_rename
func goRename(h uintptr, oldDirIno uint64, oldName *byte, oldLen int, newDirIno uint64, newName *byte, newLen int) (rc int) {
	rc = -int(kernel.EIO)
	defer kernel.CatchPanic[int](nil)
	m := getMount(h)
	od, errno := m.dir(oldDirIno)
	if errno != 0 {
		return errno
	}
	nd, errno := m.dir(newDirIno)
	if errno != 0 {
		return errno
	}
	r, ok := od.(Renamer)
	if !ok {
		return -int(kernel.EPERM)
	}
	nn := string(viewBytes(newName, newLen))
	// Replacing a directory requires it to be empty.
	target, err := nd.Lookup(nn)
	if err != nil && err != kernel.ENOENT {
		return kernel.ToErrno(err)
	}
	if target != nil {
		if td, ok := target.(Dir); ok {
			empty := true
			if err := td.ReadDir(func(string, Node) bool { empty = false; return false }); err != nil {
				return kernel.ToErrno(err)
			}
			if !empty {
				return -int(kernel.ENOTEMPTY)
			}
		}
	}
	return kernel.ToErrno(r.Rename(string(viewBytes(oldName, oldLen)), nd, nn))
}

//export gokm_fs_go_link
func goLink(h uintptr, dirIno, targetIno uint64, name *byte, nameLen int) (rc int) {
	rc = -int(kernel.EIO)
	defer kernel.CatchPanic[int](nil)
	m := getMount(h)
	d, errno := m.dir(dirIno)
	if errno != 0 {
		return errno
	}
	l, ok := d.(Linker)
	if !ok {
		return -int(kernel.EOPNOTSUPP)
	}
	n, ok := m.node(targetIno)
	if !ok {
		return -int(kernel.ENOENT)
	}
	if !withType(n, n.Attr().Mode).IsRegular() {
		return -int(kernel.EPERM)
	}
	return kernel.ToErrno(l.Link(string(viewBytes(name, nameLen)), n))
}

//export gokm_fs_go_open
func goOpen(h uintptr, ino uint64, flags uint32) (rc int) {
	rc = -int(kernel.EIO)
	defer kernel.CatchPanic[int](nil)
	n, ok := getMount(h).node(ino)
	if !ok {
		return -int(kernel.ENOENT)
	}
	if o, ok := n.(Opener); ok {
		return kernel.ToErrno(o.Open(flags))
	}
	return 0
}

//export gokm_fs_go_close
func goClose(h uintptr, ino uint64, flags uint32) {
	defer kernel.CatchPanic[int](nil)
	if n, ok := getMount(h).node(ino); ok {
		if r, ok := n.(Releaser); ok {
			r.Release(flags)
		}
	}
}

//export gokm_fs_go_symlink
func goSymlink(h uintptr, dirIno uint64, name *byte, nameLen int, target *byte, targetLen int) (rc int64) {
	rc = -int64(kernel.EIO)
	defer kernel.CatchPanic[int64](nil)
	m := getMount(h)
	d, errno := m.dir(dirIno)
	if errno != 0 {
		return int64(errno)
	}
	sl, ok := d.(Symlinker)
	if !ok {
		return -int64(kernel.EPERM)
	}
	n, err := sl.Symlink(string(viewBytes(name, nameLen)), string(viewBytes(target, targetLen)))
	if err != nil {
		return int64(kernel.ToErrno(err))
	}
	if n == nil {
		return -int64(kernel.EIO)
	}
	return int64(m.ino(n))
}

// gokm_fs_go_readlink points *buf at the link target, valid until
// GOKM_LEAVE.
//
//export gokm_fs_go_readlink
func goReadlink(h uintptr, ino uint64, buf **byte, n *int) (rc int) {
	rc = -int(kernel.EIO)
	defer kernel.CatchPanic[int](nil)
	node, ok := getMount(h).node(ino)
	if !ok {
		return -int(kernel.ENOENT)
	}
	rl, ok := node.(Readlinker)
	if !ok {
		return -int(kernel.EINVAL)
	}
	target, err := rl.Readlink()
	if err != nil {
		return kernel.ToErrno(err)
	}
	*n = len(target)
	if len(target) > 0 {
		*buf = unsafe.StringData(target)
	}
	return 0
}

//export gokm_fs_go_evict
func goEvict(h uintptr, ino uint64, unlinked bool) {
	defer kernel.CatchPanic[int](nil)
	m := getMount(h)
	if n, ok := m.node(ino); ok {
		if f, ok := n.(Forgetter); ok && unlinked {
			f.Forget()
		}
		if e, ok := n.(Evicter); ok {
			e.Evict()
		}
		if _, persistent := n.(Identifier); !unlinked && !persistent {
			return
		}
	}
	m.forget(ino)
}

//export gokm_fs_go_read
func goRead(h uintptr, ino uint64, buf *byte, count int, off int64) (rc int64) {
	rc = -int64(kernel.EIO)
	defer kernel.CatchPanic[int64](nil)
	n, ok := getMount(h).node(ino)
	if !ok {
		return -int64(kernel.ENOENT)
	}
	r, ok := n.(Reader)
	if !ok {
		return -int64(kernel.EINVAL)
	}
	got, err := r.ReadAt(viewBytes(buf, count), off)
	if err != nil && err != io.EOF && got == 0 {
		return int64(kernel.ToErrno(err))
	}
	return int64(got)
}

//export gokm_fs_go_write
func goWrite(h uintptr, ino uint64, buf *byte, count int, off int64) (rc int64) {
	rc = -int64(kernel.EIO)
	defer kernel.CatchPanic[int64](nil)
	n, ok := getMount(h).node(ino)
	if !ok {
		return -int64(kernel.ENOENT)
	}
	w, ok := n.(Writer)
	if !ok {
		return -int64(kernel.EBADF)
	}
	wrote, err := w.WriteAt(viewBytes(buf, count), off)
	if err != nil && wrote == 0 {
		return int64(kernel.ToErrno(err))
	}
	return int64(wrote)
}

//export gokm_fs_go_truncate
func goTruncate(h uintptr, ino uint64, size int64) (rc int) {
	rc = -int(kernel.EIO)
	defer kernel.CatchPanic[int](nil)
	n, ok := getMount(h).node(ino)
	if !ok {
		return -int(kernel.ENOENT)
	}
	t, ok := n.(Truncater)
	if !ok {
		return -int(kernel.EPERM)
	}
	return kernel.ToErrno(t.Truncate(size))
}

// gokm_fs_go_iterate emits directory entries from index start onwards,
// through gokm_fs_emit (which advances the kernel's position).
//
//export gokm_fs_go_iterate
func goIterate(h uintptr, dirIno uint64, start int64, ctx uintptr) (rc int) {
	rc = -int(kernel.EIO)
	defer kernel.CatchPanic[int](nil)
	m := getMount(h)
	d, errno := m.dir(dirIno)
	if errno != 0 {
		return errno
	}
	if start < 0 {
		return -int(kernel.EINVAL)
	}
	if reader, ok := d.(DirReader); ok {
		var emitErr int
		cookie := uint64(start)
		err := reader.ReadDirFrom(cookie, func(next uint64, name string, n Node) bool {
			if next <= cookie || next > uint64(^uint64(0)>>1)-2 || n == nil {
				emitErr = -int(kernel.EINVAL)
				return false
			}
			ino := m.entryIno(n)
			if ino < 0 {
				emitErr = int(ino)
				return false
			}
			mode := withType(n, n.Attr().Mode)
			if !cEmitCookie(ctx, unsafe.StringData(name), len(name), uint64(ino), uint32(mode&ModeType)>>12, int64(next)+2) {
				return false
			}
			cookie = next
			return true
		})
		if emitErr != 0 {
			return emitErr
		}
		return kernel.ToErrno(err)
	}
	var i int64
	var iterateErr int
	err := d.ReadDir(func(name string, n Node) bool {
		if i++; i <= start {
			return true
		}
		if n == nil {
			return true // skip: entries need a node (and inode number)
		}
		mode := withType(n, n.Attr().Mode)
		// Persistent entries need no node-cache slot merely to report an ID.
		// Otherwise a large readdir would retain every Node without iget/evict.
		ino := m.entryIno(n)
		if ino < 0 {
			iterateErr = int(ino)
			return false
		}
		return cEmit(ctx, unsafe.StringData(name), len(name), uint64(ino), uint32(mode&ModeType)>>12)
	})
	if iterateErr != 0 {
		return iterateErr
	}
	return kernel.ToErrno(err)
}

// Extended attributes. Buffers are kernel memory, valid during the call.

const (
	xattrCreate  = 1 // XATTR_CREATE
	xattrReplace = 2 // XATTR_REPLACE
)

func xattrer(h uintptr, ino uint64) (Xattrer, int) {
	n, ok := getMount(h).node(ino)
	if !ok {
		return nil, -int(kernel.ENOENT)
	}
	x, ok := n.(Xattrer)
	if !ok {
		return nil, -int(kernel.EOPNOTSUPP)
	}
	return x, 0
}

// gokm_fs_go_getxattr copies the value into buf and returns its length;
// with size 0 it only returns the length.
//
//export gokm_fs_go_getxattr
func goGetxattr(h uintptr, ino uint64, name *byte, nameLen int, buf *byte, size int) (rc int) {
	rc = -int(kernel.EIO)
	defer kernel.CatchPanic[int](nil)
	x, errno := xattrer(h, ino)
	if errno != 0 {
		if errno == -int(kernel.EOPNOTSUPP) {
			return -int(kernel.ENODATA)
		}
		return errno
	}
	v, err := x.GetXattr(string(viewBytes(name, nameLen)))
	if err != nil {
		return kernel.ToErrno(err)
	}
	if size == 0 {
		return len(v)
	}
	if len(v) > size {
		return -int(kernel.ERANGE)
	}
	copy(viewBytes(buf, size), v)
	return len(v)
}

//export gokm_fs_go_setxattr
func goSetxattr(h uintptr, ino uint64, name *byte, nameLen int, value *byte, size int,
	remove bool, flags int32) (rc int) {
	rc = -int(kernel.EIO)
	defer kernel.CatchPanic[int](nil)
	x, errno := xattrer(h, ino)
	if errno != 0 {
		return errno
	}
	key := string(viewBytes(name, nameLen)) // a copy: implementations may keep it
	if remove {
		return kernel.ToErrno(x.RemoveXattr(key))
	}
	if flags&(xattrCreate|xattrReplace) != 0 {
		_, err := x.GetXattr(key)
		exists := err == nil
		if err != nil && err != kernel.ENODATA {
			return kernel.ToErrno(err)
		}
		if flags&xattrCreate != 0 && exists {
			return -int(kernel.EEXIST)
		}
		if flags&xattrReplace != 0 && !exists {
			return -int(kernel.ENODATA)
		}
	}
	v := make([]byte, size)
	copy(v, viewBytes(value, size))
	return kernel.ToErrno(x.SetXattr(key, v))
}

// gokm_fs_go_listxattr writes the names, each NUL-terminated, into buf and
// returns the total length (only the length when size is 0).
//
//export gokm_fs_go_listxattr
func goListxattr(h uintptr, ino uint64, buf *byte, size int) (rc int) {
	rc = -int(kernel.EIO)
	defer kernel.CatchPanic[int](nil)
	x, errno := xattrer(h, ino)
	if errno != 0 {
		if errno == -int(kernel.EOPNOTSUPP) {
			return 0
		}
		return errno
	}
	names, err := x.ListXattrs()
	if err != nil {
		return kernel.ToErrno(err)
	}
	total := 0
	for _, n := range names {
		total += len(n) + 1
	}
	if size == 0 {
		return total
	}
	if total > size {
		return -int(kernel.ERANGE)
	}
	out := viewBytes(buf, size)
	for _, n := range names {
		copy(out, n)
		out[len(n)] = 0
		out = out[len(n)+1:]
	}
	return total
}
