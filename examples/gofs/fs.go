package main

import (
	"sync"

	"gokm/fs"
	"gokm/kernel"
)

// mu protects the whole tree: the kernel calls these methods concurrently
// from several CPUs.
var mu sync.Mutex

// dir is a directory: entries in creation order.
type dir struct {
	xattrs
	mode     fs.Mode
	names    []string
	children map[string]fs.Node
}

func newDir(mode fs.Mode) *dir {
	return &dir{mode: mode & fs.ModePerm, children: map[string]fs.Node{}}
}

func (d *dir) Attr() fs.Attr { return fs.Attr{Mode: fs.ModeDir | d.mode} }

func (d *dir) Lookup(name string) (fs.Node, error) {
	mu.Lock()
	defer mu.Unlock()
	return d.children[name], nil
}

func (d *dir) ReadDir(yield func(string, fs.Node) bool) error {
	// Snapshot under the lock, yield without it (yield calls back into
	// methods that take it, like Attr).
	mu.Lock()
	names := append([]string(nil), d.names...)
	nodes := make([]fs.Node, len(names))
	for i, name := range names {
		nodes[i] = d.children[name]
	}
	mu.Unlock()
	for i, name := range names {
		if !yield(name, nodes[i]) {
			break
		}
	}
	return nil
}

func (d *dir) add(name string, n fs.Node) error {
	if len(name) > 255 {
		return kernel.ENAMETOOLONG
	}
	if d.children[name] != nil {
		return kernel.EEXIST
	}
	d.children[name] = n
	d.names = append(d.names, name)
	return nil
}

func (d *dir) Create(name string, mode fs.Mode) (fs.Node, error) {
	mu.Lock()
	defer mu.Unlock()
	f := &file{mode: mode & fs.ModePerm, links: 1}
	return f, d.add(name, f)
}

func (d *dir) Mkdir(name string, mode fs.Mode) (fs.Dir, error) {
	mu.Lock()
	defer mu.Unlock()
	sub := newDir(mode)
	return sub, d.add(name, sub)
}

func (d *dir) Remove(name string) error {
	mu.Lock()
	defer mu.Unlock()
	if f, ok := d.children[name].(*file); ok {
		f.links--
	}
	return d.removeLocked(name)
}

func (d *dir) Link(name string, target fs.Node) error {
	mu.Lock()
	defer mu.Unlock()
	f, ok := target.(*file)
	if !ok {
		return kernel.EPERM
	}
	if err := d.add(name, f); err != nil {
		return err
	}
	f.links++
	return nil
}

func (d *dir) removeLocked(name string) error {
	if d.children[name] == nil {
		return kernel.ENOENT
	}
	delete(d.children, name)
	for i, n := range d.names {
		if n == name {
			d.names = append(d.names[:i], d.names[i+1:]...)
			break
		}
	}
	return nil
}

func (d *dir) Rename(oldName string, newDir fs.Dir, newName string) error {
	mu.Lock()
	defer mu.Unlock()
	nd := newDir.(*dir)
	n := d.children[oldName]
	if n == nil {
		return kernel.ENOENT
	}
	if nd.children[newName] == n {
		return nil
	}
	if f, ok := nd.children[newName].(*file); ok {
		f.links--
	}
	if nd.children[newName] != nil {
		nd.removeLocked(newName) // replaced
	}
	d.removeLocked(oldName)
	return nd.add(newName, n)
}

func (d *dir) Symlink(name, target string) (fs.Node, error) {
	mu.Lock()
	defer mu.Unlock()
	l := &symlink{target: target}
	return l, d.add(name, l)
}

// symlink is a symbolic link.
type symlink struct{ target string }

func (l *symlink) Attr() fs.Attr {
	return fs.Attr{Mode: fs.ModeSymlink | 0o777, Size: int64(len(l.target))}
}

func (l *symlink) Readlink() (string, error) { return l.target, nil }

// file is a regular file held in a Go byte slice.
type file struct {
	xattrs
	mode  fs.Mode
	links uint32
	data  []byte
}

func (f *file) Attr() fs.Attr {
	mu.Lock()
	defer mu.Unlock()
	return fs.Attr{Mode: fs.ModeRegular | f.mode, Size: int64(len(f.data)), Nlink: f.links}
}

func (f *file) ReadAt(p []byte, off int64) (int, error) {
	mu.Lock()
	defer mu.Unlock()
	if off >= int64(len(f.data)) {
		return 0, nil
	}
	return copy(p, f.data[off:]), nil
}

func (f *file) WriteAt(p []byte, off int64) (int, error) {
	mu.Lock()
	defer mu.Unlock()
	end := off + int64(len(p))
	if !f.reserve(end) {
		return 0, kernel.ENOSPC
	}
	if l := int64(len(f.data)); end > l {
		f.data = f.data[:end]
		clear(f.data[l:off]) // a hole reads as zeros (no-op when off <= l)
	}
	return copy(f.data[off:], p), nil
}

func (f *file) Truncate(size int64) error {
	mu.Lock()
	defer mu.Unlock()
	if l := int64(len(f.data)); size > l {
		if !f.reserve(size) {
			return kernel.ENOSPC
		}
		f.data = f.data[:size]
		clear(f.data[l:])
		return nil
	}
	f.data = f.data[:size]
	if size < int64(cap(f.data))/4 { // give memory back to the GC heap
		f.data = append([]byte(nil), f.data...)
	}
	return nil
}

// reserve makes cap(f.data) >= size, failing (ENOSPC for the caller) when
// the Go heap can't hold it: kernel.MakeBytes returns an error instead of
// the fatal out-of-memory error of make.
func (f *file) reserve(size int64) bool {
	if size <= int64(cap(f.data)) {
		return true
	}
	nd, err := kernel.MakeBytes(int(max(2*int64(cap(f.data)), size)))
	if err != nil {
		nd, err = kernel.MakeBytes(int(size)) // without headroom
	}
	if err != nil {
		return false
	}
	nd = nd[:len(f.data)]
	copy(nd, f.data)
	f.data = nd
	return true
}

// xattrs holds a node's extended attributes (fs.Xattrer), under mu.
type xattrs struct {
	attrs map[string][]byte
}

func (x *xattrs) GetXattr(name string) ([]byte, error) {
	mu.Lock()
	defer mu.Unlock()
	v, ok := x.attrs[name]
	if !ok {
		return nil, kernel.ENODATA
	}
	return v, nil
}

func (x *xattrs) SetXattr(name string, value []byte) error {
	mu.Lock()
	defer mu.Unlock()
	if x.attrs == nil {
		x.attrs = map[string][]byte{}
	}
	x.attrs[name] = value
	return nil
}

func (x *xattrs) RemoveXattr(name string) error {
	mu.Lock()
	defer mu.Unlock()
	if _, ok := x.attrs[name]; !ok {
		return kernel.ENODATA
	}
	delete(x.attrs, name)
	return nil
}

func (x *xattrs) ListXattrs() ([]string, error) {
	mu.Lock()
	defer mu.Unlock()
	names := make([]string, 0, len(x.attrs))
	for n := range x.attrs {
		names = append(names, n)
	}
	return names, nil
}
