package fs

import (
	"sync"
	"testing"
	"unsafe"

	"gokm/kernel"
)

var foreignCalls, foreignResult, emitted int
var lastFlush bool
var lastFlags uint32
var lastCookie int64

type node struct {
	id                                uint64
	forgotten, evicted, opens, closes int
}

func (n *node) InodeID() uint64   { return n.id }
func (n *node) Attr() Attr        { return Attr{Mode: ModeRegular | 0600, Nlink: 2, Generation: 7} }
func (n *node) Forget()           { n.forgotten++ }
func (n *node) Evict()            { n.evicted++ }
func (n *node) Open(uint32) error { n.opens++; return nil }
func (n *node) Release(uint32)    { n.closes++ }

type directory struct {
	entries map[string]Node
	syncs   int
	err     error
}

func (d *directory) Attr() Attr                    { return Attr{Mode: ModeDir | 0700} }
func (d *directory) Lookup(s string) (Node, error) { return d.entries[s], nil }
func (d *directory) ReadDir(yield func(string, Node) bool) error {
	for name, n := range d.entries {
		if !yield(name, n) {
			break
		}
	}
	return nil
}
func (d *directory) Sync() error { d.syncs++; return d.err }
func (d *directory) Link(name string, n Node) error {
	if d.entries[name] != nil {
		return kernel.EEXIST
	}
	d.entries[name] = n
	return nil
}

type syncedNode struct {
	node
	start, end int64
	data       bool
	calls      int
	err        error
}

func (n *syncedNode) Fsync(start, end int64, data bool) error {
	n.calls++
	n.start, n.end, n.data = start, end, data
	return n.err
}

func mounted(t *testing.T) (*mount, uintptr, *directory) {
	t.Helper()
	d := &directory{entries: map[string]Node{}}
	m := &mount{root: d, nodes: map[uint64]Node{}, inos: map[nodeKey]uint64{}, next: 1}
	if m.ino(d) != 1 {
		t.Fatal("root")
	}
	h := kernel.NewHandle(m)
	t.Cleanup(h.Delete)
	return m, uintptr(h), d
}
func TestPersistentIdentityAndEviction(t *testing.T) {
	m, h, _ := mounted(t)
	n := &node{id: 42}
	if m.ino(n) != 42 || m.ino(n) != 42 {
		t.Fatal("unstable inode")
	}
	if m.ino(&node{id: 42}) != -int64(kernel.EEXIST) {
		t.Fatal("ID collision accepted")
	}
	if m.ino(&node{id: 0}) != -int64(kernel.EINVAL) {
		t.Fatal("invalid ID")
	}
	if m.ino(&node{id: 1 << 63}) != -int64(kernel.EINVAL) {
		t.Fatal("overflow ID")
	}
	goEvict(h, 42, false)
	if n.forgotten != 0 || n.evicted != 1 {
		t.Fatal("linked eviction freed storage")
	}
	reloaded := &node{id: 42}
	if m.ino(reloaded) != 42 {
		t.Fatal("cannot reconstruct persistent inode")
	}
	goEvict(h, 42, true)
	if reloaded.forgotten != 1 || reloaded.evicted != 1 {
		t.Fatal("final unlink hooks")
	}
}
func TestSyncDispatchAndErrors(t *testing.T) {
	m, h, d := mounted(t)
	n := &syncedNode{node: node{id: 8}, err: kernel.EIO}
	m.ino(n)
	var handled bool
	if got := goFsync(h, 8, 10, 99, true, &handled); got != -int(kernel.EIO) || !handled {
		t.Fatalf("got %d handled=%v", got, handled)
	}
	if n.calls != 1 || n.start != 10 || n.end != 99 || !n.data || d.syncs != 0 {
		t.Fatal("per-node fsync did not isolate scope")
	}
	if got := goFsync(h, 1, 0, 1<<63-1, false, &handled); got != 0 || handled || d.syncs != 1 {
		t.Fatal("legacy directory fallback")
	}
	d.err = kernel.ENOSPC
	if got := goFsync(h, 1, 0, 1<<63-1, false, &handled); got != -int(kernel.ENOSPC) {
		t.Fatal("fallback swallowed error")
	}
	if got := goFsync(h, 999, 0, 0, false, &handled); got != -int(kernel.ENOENT) {
		t.Fatal("missing inode")
	}
}
func TestLinkAndLifecycleDispatch(t *testing.T) {
	m, h, d := mounted(t)
	n := &node{id: 8}
	m.ino(n)
	name := []byte("alias")
	if got := goLink(h, 1, 8, unsafe.SliceData(name), len(name)); got != 0 || d.entries["alias"] != n {
		t.Fatal("hard link did not retain identity")
	}
	if got := goLink(h, 1, 8, unsafe.SliceData(name), len(name)); got != -int(kernel.EEXIST) {
		t.Fatal("duplicate link")
	}
	if got := goLink(h, 1, 1, unsafe.SliceData(name), len(name)); got != -int(kernel.EPERM) {
		t.Fatal("directory hard link")
	}
	if goOpen(h, 8, 2) != 0 {
		t.Fatal("open")
	}
	goClose(h, 8, 2)
	if n.opens != 1 || n.closes != 1 {
		t.Fatal("lifecycle dispatch")
	}
}
func TestDirectoryIDsDoNotPopulateCache(t *testing.T) {
	m, h, d := mounted(t)
	for i := uint64(2); i < 1002; i++ {
		d.entries[string(rune(i))] = &node{id: i}
	}
	emitted = 0
	if goIterate(h, 1, 0, 0) != 0 || emitted != 1000 {
		t.Fatal("iterate")
	}
	if len(m.nodes) != 1 {
		t.Fatalf("readdir retained %d nodes", len(m.nodes))
	}
}

type resumableDir struct {
	directory
	seen    uint64
	invalid bool
}

func (d *resumableDir) ReadDirFrom(cookie uint64, yield func(uint64, string, Node) bool) error {
	d.seen = cookie
	next := cookie + 7
	if d.invalid {
		next = cookie
	}
	yield(next, "entry", &node{id: 99})
	return nil
}
func TestResumableDirectory(t *testing.T) {
	m, h, _ := mounted(t)
	d := &resumableDir{}
	ino := m.ino(d)
	emitted = 0
	if goIterate(h, uint64(ino), 10, 0) != 0 || d.seen != 10 || lastCookie != 19 || emitted != 1 {
		t.Fatal("cookie not resumed")
	}
	d.invalid = true
	if goIterate(h, uint64(ino), 10, 0) != -int(kernel.EINVAL) {
		t.Fatal("non-increasing cookie accepted")
	}
}
func TestConcurrentIdentity(t *testing.T) {
	m, _, _ := mounted(t)
	n := &node{id: 99}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if m.ino(n) != 99 {
					t.Error("identity")
				}
			}
		}()
	}
	wg.Wait()
}
func TestDeviceBoundsAndPersistenceFlags(t *testing.T) {
	d := &Device{blockSize: 4096, size: 16384}
	p := make([]byte, 4096)
	foreignCalls, foreignResult = 0, 0
	for _, block := range []uint64{4, 5, ^uint64(0)} {
		if d.ReadBlocks(block, p) != kernel.EINVAL || d.WriteBlocks(block, p) != kernel.EINVAL || d.WriteDirect(block, p, 0) != kernel.EINVAL {
			t.Fatal("out of bounds")
		}
	}
	if foreignCalls != 0 {
		t.Fatal("invalid range reached device")
	}
	if d.WriteDirect(3, p, WritePreflush|WriteFUA) != nil || lastFlags != 3 {
		t.Fatal("write flags")
	}
	if d.WriteDirect(0, p, WriteFlags(4)) != kernel.EINVAL {
		t.Fatal("unknown flags")
	}
	if d.Sync() != nil || !lastFlush {
		t.Fatal("Sync lacks flush")
	}
	if d.Writeback() != nil || lastFlush {
		t.Fatal("Writeback unexpectedly flushes")
	}
	foreignResult = -int(kernel.EIO)
	if d.Sync() != kernel.EIO || d.ReadDirect(0, p) != kernel.EIO {
		t.Fatal("lost device error")
	}
	d.readOnly = true
	if d.WriteDirect(0, p, 0) != kernel.EROFS || d.WriteBlocks(0, p) != kernel.EROFS {
		t.Fatal("read-only write")
	}
}

func TestMountOptionsAndRootValidation(t *testing.T) {
	var seen MountOptions
	d := &directory{entries: map[string]Node{}}
	f := &FileSystem{MountWithOptions: func(opts MountOptions) (Dir, error) { seen = opts; return d, nil }}
	h := kernel.NewHandle(f)
	defer h.Delete()
	options := []byte("uid=123,policy=streams")
	var out uintptr
	if rc := goMount(uintptr(h), 0, 4096, 0, true, unsafe.SliceData(options), len(options), &out); rc != 0 {
		t.Fatalf("mount=%d", rc)
	}
	defer goRelease(out)
	if !seen.ReadOnly || seen.Options != string(options) {
		t.Fatalf("options=%+v", seen)
	}
	options[0] = 'X'
	if seen.Options[0] == 'X' {
		t.Fatal("mount options retained foreign buffer")
	}
	f.MountWithOptions = nil
	f.Mount = func() (Dir, error) { return d, nil }
	if rc := goMount(uintptr(h), 0, 4096, 0, false, unsafe.SliceData(options), len(options), &out); rc != -int(kernel.EINVAL) {
		t.Fatal("legacy mount ignored options")
	}
}

func TestPersistentAttributes(t *testing.T) {
	m, h, _ := mounted(t)
	n := &node{id: 22}
	m.ino(n)
	var mode, links, generation, uid, gid uint32
	var owner bool
	var size, atime, mtime, ctime int64
	if goStat(h, 22, &mode, &links, &generation, &uid, &gid, &owner, &size, &atime, &mtime, &ctime) != 0 {
		t.Fatal("stat")
	}
	if links != 2 || generation != 7 || owner {
		t.Fatal("persistent attributes")
	}
}
