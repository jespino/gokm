// Command fstest exercises gofs through real syscalls. It's built with the
// regular Go toolchain and run inside the QEMU test VM after gofs.ko is
// loaded (test/qemu/run.sh -t examples/gofs/fstest). Exits non-zero on
// failure.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unsafe"
)

const mnt = "/mnt"

var failed bool

func check(name string, err error) {
	if err != nil {
		fmt.Printf("fstest: FAIL %s: %v\n", name, err)
		failed = true
		return
	}
	fmt.Printf("fstest: ok   %s\n", name)
}

func expectErrno(err error, want syscall.Errno) error {
	if !errors.Is(err, want) {
		return fmt.Errorf("got %v, want %v", err, want)
	}
	return nil
}

func expectContent(path string, want []byte) error {
	got, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("content %q, want %q", trunc(got), trunc(want))
	}
	return nil
}

func trunc(b []byte) string {
	if len(b) > 40 {
		return string(b[:40]) + "..."
	}
	return string(b)
}

func p(name string) string { return filepath.Join(mnt, name) }

func main() {
	if len(os.Args) > 1 && os.Args[1] == "exec-probe" {
		fmt.Println("exec-probe: running from " + os.Args[0])
		return
	}
	os.MkdirAll(mnt, 0o755)
	check("mount", syscall.Mount("none", mnt, "gofs", 0, ""))

	check("statfs magic", func() error {
		var st syscall.Statfs_t
		if err := syscall.Statfs(mnt, &st); err != nil {
			return err
		}
		if st.Type != 0x676f6673 {
			return fmt.Errorf("f_type %#x", st.Type)
		}
		return nil
	}())

	check("write+read file", func() error {
		if err := os.WriteFile(p("hello.txt"), []byte("hello gofs\n"), 0o644); err != nil {
			return err
		}
		return expectContent(p("hello.txt"), []byte("hello gofs\n"))
	}())

	check("append", func() error {
		f, err := os.OpenFile(p("hello.txt"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		f.WriteString("more\n")
		f.Close()
		return expectContent(p("hello.txt"), []byte("hello gofs\nmore\n"))
	}())

	check("truncate shrink/extend", func() error {
		if err := os.Truncate(p("hello.txt"), 5); err != nil {
			return err
		}
		if err := expectContent(p("hello.txt"), []byte("hello")); err != nil {
			return err
		}
		if err := os.Truncate(p("hello.txt"), 8); err != nil {
			return err
		}
		return expectContent(p("hello.txt"), []byte("hello\x00\x00\x00"))
	}())

	check("O_TRUNC rewrite", func() error {
		if err := os.WriteFile(p("hello.txt"), []byte("new"), 0o644); err != nil {
			return err
		}
		return expectContent(p("hello.txt"), []byte("new"))
	}())

	check("sparse write (hole reads as zeros)", func() error {
		f, err := os.Create(p("sparse"))
		if err != nil {
			return err
		}
		f.WriteAt([]byte("X"), 100)
		f.Close()
		want := make([]byte, 101)
		want[100] = 'X'
		return expectContent(p("sparse"), want)
	}())

	check("1 MiB file round trip", func() error {
		big := make([]byte, 1<<20)
		for i := range big {
			big[i] = byte(i * 7)
		}
		if err := os.WriteFile(p("big"), big, 0o644); err != nil {
			return err
		}
		if fi, err := os.Stat(p("big")); err != nil || fi.Size() != 1<<20 {
			return fmt.Errorf("stat: %v %v", fi, err)
		}
		return expectContent(p("big"), big)
	}())

	check("directories + readdir", func() error {
		if err := os.Mkdir(p("dir"), 0o755); err != nil {
			return err
		}
		for _, n := range []string{"c", "a", "b"} {
			if err := os.WriteFile(p("dir/"+n), []byte(n), 0o644); err != nil {
				return err
			}
		}
		if err := os.Mkdir(p("dir/sub"), 0o700); err != nil {
			return err
		}
		ents, err := os.ReadDir(p("dir"))
		if err != nil {
			return err
		}
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		if strings.Join(names, ",") != "a,b,c,sub" {
			return fmt.Errorf("entries %v", names)
		}
		if !ents[3].IsDir() || ents[0].IsDir() {
			return errors.New("wrong entry types")
		}
		return expectContent(p("dir/b"), []byte("b"))
	}())

	check("EEXIST / ENOENT / ENOTEMPTY / ENOTDIR", func() error {
		if err := expectErrno(os.Mkdir(p("dir"), 0o755), syscall.EEXIST); err != nil {
			return err
		}
		if _, err := os.Open(p("missing")); !errors.Is(err, syscall.ENOENT) {
			return fmt.Errorf("open missing: %v", err)
		}
		if err := expectErrno(syscall.Rmdir(p("dir")), syscall.ENOTEMPTY); err != nil {
			return err
		}
		return expectErrno(os.Mkdir(p("hello.txt/x"), 0o755), syscall.ENOTDIR)
	}())

	check("unlinked open file stays readable", func() error {
		os.WriteFile(p("ghost"), []byte("boo"), 0o644)
		f, err := os.Open(p("ghost"))
		if err != nil {
			return err
		}
		defer f.Close()
		if err := os.Remove(p("ghost")); err != nil {
			return err
		}
		if _, err := os.Stat(p("ghost")); !errors.Is(err, syscall.ENOENT) {
			return fmt.Errorf("stat after unlink: %v", err)
		}
		b, err := io.ReadAll(f)
		if err != nil || string(b) != "boo" {
			return fmt.Errorf("read %q %v", b, err)
		}
		return nil
	}())

	check("fill heap -> ENOSPC, then reclaim", func() error {
		chunk := bytes.Repeat([]byte("z"), 256<<10)
		f, err := os.Create(p("hog"))
		if err != nil {
			return err
		}
		var werr error
		written := 0
		for i := 0; i < 200 && werr == nil; i++ {
			var n int
			n, werr = f.Write(chunk)
			written += n
		}
		f.Close()
		if err := expectErrno(werr, syscall.ENOSPC); err != nil {
			return err
		}
		fmt.Printf("fstest:      filled %d KiB before ENOSPC\n", written>>10)
		if err := os.Remove(p("hog")); err != nil {
			return err
		}
		// The space must be reusable after the file is gone.
		return os.WriteFile(p("after-hog"), chunk, 0o644)
	}())

	check("300 files", func() error {
		for i := 0; i < 300; i++ {
			if err := os.WriteFile(p(fmt.Sprintf("dir/sub/f%03d", i)), []byte{byte(i)}, 0o644); err != nil {
				return err
			}
		}
		ents, err := os.ReadDir(p("dir/sub"))
		if err != nil {
			return err
		}
		if len(ents) != 300 {
			return fmt.Errorf("%d entries", len(ents))
		}
		for i := 0; i < 300; i++ {
			if err := os.Remove(p(fmt.Sprintf("dir/sub/f%03d", i))); err != nil {
				return err
			}
		}
		return nil
	}())

	check("remove tree", func() error {
		if err := os.RemoveAll(p("dir")); err != nil {
			return err
		}
		ents, err := os.ReadDir(mnt)
		if err != nil {
			return err
		}
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		if strings.Join(names, ",") != "after-hog,big,hello.txt,sparse" {
			return fmt.Errorf("root entries %v", names)
		}
		return nil
	}())

	// O_NONBLOCK delete_module of an in-use module fails with EWOULDBLOCK.
	check("rename: same dir, across dirs, replacing", func() error {
		os.Mkdir(p("r1"), 0o755)
		os.Mkdir(p("r2"), 0o755)
		os.WriteFile(p("r1/a"), []byte("A"), 0o644)
		os.WriteFile(p("r2/b"), []byte("B"), 0o644)
		if err := os.Rename(p("r1/a"), p("r1/a2")); err != nil {
			return err
		}
		if err := os.Rename(p("r1/a2"), p("r2/b")); err != nil { // replaces b
			return err
		}
		if _, err := os.Stat(p("r1/a2")); !errors.Is(err, syscall.ENOENT) {
			return fmt.Errorf("old name still there: %v", err)
		}
		if err := expectContent(p("r2/b"), []byte("A")); err != nil {
			return err
		}
		ents, _ := os.ReadDir(p("r2"))
		if len(ents) != 1 {
			return fmt.Errorf("r2 has %d entries", len(ents))
		}
		if err := os.Rename(p("r2"), p("r3")); err != nil { // a directory
			return err
		}
		if err := expectContent(p("r3/b"), []byte("A")); err != nil {
			return err
		}
		os.WriteFile(p("r1/x"), nil, 0o644)
		// os.Rename refuses directory targets itself (EEXIST): use the syscall.
		if err := expectErrno(syscall.Rename(p("r3"), p("r1")), syscall.ENOTEMPTY); err != nil {
			return err
		}
		return os.RemoveAll(p("r1"))
	}())

	check("symlinks", func() error {
		if err := os.Symlink("r3/b", p("link")); err != nil {
			return err
		}
		target, err := os.Readlink(p("link"))
		if err != nil || target != "r3/b" {
			return fmt.Errorf("readlink %q %v", target, err)
		}
		fi, err := os.Lstat(p("link"))
		if err != nil || fi.Mode()&os.ModeSymlink == 0 || fi.Size() != 4 {
			return fmt.Errorf("lstat %v %v", fi.Mode(), err)
		}
		if err := expectContent(p("link"), []byte("A")); err != nil { // followed
			return err
		}
		if err := os.Remove(p("link")); err != nil {
			return err
		}
		return os.RemoveAll(p("r3"))
	}())

	storageContractTests()
	mmapTests()
	xattrTests()

	check("rmmod refused while mounted", expectErrno(deleteModule("gofs"), syscall.EWOULDBLOCK))

	check("umount", syscall.Unmount(mnt, 0))

	check("remount is a fresh, empty fs", func() error {
		if err := syscall.Mount("none", mnt, "gofs", 0, ""); err != nil {
			return err
		}
		ents, err := os.ReadDir(mnt)
		if err != nil {
			return err
		}
		if len(ents) != 0 {
			return fmt.Errorf("%d entries", len(ents))
		}
		return syscall.Unmount(mnt, 0)
	}())

	if failed {
		os.Exit(1)
	}
}

func deleteModule(name string) error {
	b, _ := syscall.BytePtrFromString(name)
	_, _, e := syscall.Syscall(syscall.SYS_DELETE_MODULE, uintptr(unsafe.Pointer(b)), syscall.O_NONBLOCK, 0)
	if e != 0 {
		return e
	}
	return nil
}
