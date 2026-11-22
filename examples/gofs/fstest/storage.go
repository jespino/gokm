package main

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

func nlink(path string, want uint64) error {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return err
	}
	if st.Nlink != want {
		return fmt.Errorf("%s links=%d want=%d", path, st.Nlink, want)
	}
	return nil
}

func storageContractTests() {
	check("hard links: alias identity, write, unlink, eviction", func() error {
		dir := p("hardlinks")
		if err := os.Mkdir(dir, 0700); err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		a, b := dir+"/a", dir+"/b"
		if err := os.WriteFile(a, []byte("original"), 0600); err != nil {
			return err
		}
		if err := os.Link(a, b); err != nil {
			return err
		}
		if err := expectErrno(os.Link(a, b), syscall.EEXIST); err != nil {
			return err
		}
		x, err := os.Stat(a)
		if err != nil {
			return err
		}
		y, err := os.Stat(b)
		if err != nil {
			return err
		}
		if !os.SameFile(x, y) {
			return fmt.Errorf("aliases have different inodes")
		}
		if err := nlink(b, 2); err != nil {
			return err
		}
		if err := os.WriteFile(b, []byte("changed"), 0600); err != nil {
			return err
		}
		if err := expectContent(a, []byte("changed")); err != nil {
			return err
		}
		if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0600); err != nil {
			return err
		}
		if err := nlink(b, 2); err != nil {
			return err
		}
		if err := os.Remove(a); err != nil {
			return err
		}
		if err := nlink(b, 1); err != nil {
			return err
		}
		return expectContent(b, []byte("changed"))
	}())
	check("hard links: rename aliases and replace linked target", func() error {
		dir := p("linkrename")
		if err := os.Mkdir(dir, 0700); err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		a, a2, b, b2 := dir+"/a", dir+"/a2", dir+"/b", dir+"/b2"
		if err := os.WriteFile(a, []byte("A"), 0600); err != nil {
			return err
		}
		if err := os.WriteFile(b, []byte("B"), 0600); err != nil {
			return err
		}
		if err := os.Link(a, a2); err != nil {
			return err
		}
		if err := os.Link(b, b2); err != nil {
			return err
		}
		old, err := os.Open(b)
		if err != nil {
			return err
		}
		defer old.Close()
		if err := os.Rename(a, b); err != nil {
			return err
		}
		if err := nlink(b2, 1); err != nil {
			return err
		}
		if err := nlink(b, 2); err != nil {
			return err
		}
		if err := expectContent(b2, []byte("B")); err != nil {
			return err
		}
		if err := os.Rename(b, a2); err != nil {
			return err
		}
		if err := nlink(b, 2); err != nil {
			return err
		}
		if err := expectContent(a2, []byte("A")); err != nil {
			return err
		}
		if err := os.Remove(b2); err != nil {
			return err
		}
		got, err := io.ReadAll(old)
		if err != nil {
			return err
		}
		if string(got) != "B" {
			return fmt.Errorf("replaced inode contents=%q", got)
		}
		return nil
	}())
	check("directory fsync and fdatasync", func() error {
		d, err := os.Open(mnt)
		if err != nil {
			return err
		}
		defer d.Close()
		if err := d.Sync(); err != nil {
			return err
		}
		f, err := os.Create(p("sync-file"))
		if err != nil {
			return err
		}
		defer f.Close()
		defer os.Remove(f.Name())
		if _, err := f.WriteString("durability callback"); err != nil {
			return err
		}
		if err := syscall.Fdatasync(int(f.Fd())); err != nil {
			return err
		}
		return f.Sync()
	}())
	check("flock excludes a second open description", func() error {
		path := p("lock-file")
		a, err := os.Create(path)
		if err != nil {
			return err
		}
		defer a.Close()
		defer os.Remove(path)
		b, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer b.Close()
		if err := syscall.Flock(int(a.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return err
		}
		if err := expectErrno(syscall.Flock(int(b.Fd()), syscall.LOCK_EX|syscall.LOCK_NB), syscall.EWOULDBLOCK); err != nil {
			return err
		}
		if err := syscall.Flock(int(a.Fd()), syscall.LOCK_UN); err != nil {
			return err
		}
		return syscall.Flock(int(b.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}())
}
