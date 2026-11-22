// Run only in a disposable QEMU guest: this mounts an intentionally destructive
// fixture filesystem on a fresh brd RAM disk.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"unsafe"
)

const mnt = "/mnt/fsblock"

var failed bool

func check(name string, fn func() error) {
	if err := fn(); err != nil {
		fmt.Printf("fsblocktest: FAIL %s: %v\n", name, err)
		failed = true
	} else {
		fmt.Printf("fsblocktest: ok %s\n", name)
	}
}
func pattern(i int) byte { return byte(i*31 + (i/4096)*17 + 7) }
func expected() []byte {
	b := make([]byte, 257*4096-37)
	for i := range b {
		if i < 128*4096 || i >= 129*4096 {
			b[i] = pattern(i)
		}
	}
	return b
}
func errno(err error, want syscall.Errno) error {
	if !errors.Is(err, want) {
		return fmt.Errorf("got %v want %v", err, want)
	}
	return nil
}
func main() {
	os.MkdirAll(mnt, 0700)
	check("load disposable brd", func() error {
		f, err := os.Open("/extra/brd.ko.zst")
		if os.IsNotExist(err) {
			_, err = os.Stat("/dev/ram0")
			return err
		}
		if err != nil {
			return err
		}
		defer f.Close()
		params := []byte("rd_nr=1 rd_size=16384 max_part=0\x00")
		_, _, e := syscall.Syscall(313, f.Fd(), uintptr(unsafe.Pointer(&params[0])), 4)
		if e != 0 {
			return e
		}
		return nil
	})
	check("reject unknown mount options", func() error { return errno(syscall.Mount("/dev/ram0", mnt, "fsblock", 0, "unknown=1"), syscall.EINVAL) })
	check("mount, direct bios and persistence barriers", func() error {
		return syscall.Mount("/dev/ram0", mnt, "fsblock", 0, "fixture=overwrite-disposable-device")
	})
	if failed {
		os.Exit(1)
	}
	check("configured mount ownership", func() error {
		var st syscall.Stat_t
		if err := syscall.Stat(mnt, &st); err != nil {
			return err
		}
		if st.Uid != 123 || st.Gid != 456 {
			return fmt.Errorf("owner=%d:%d", st.Uid, st.Gid)
		}
		return nil
	})
	check("native cached reads and sparse extents", func() error {
		got, err := os.ReadFile(mnt + "/sealed")
		if err != nil {
			return err
		}
		if !bytes.Equal(got, expected()) {
			return fmt.Errorf("native data differs")
		}
		return nil
	})
	check("mmap, pread, private writes and sealed protection", func() error {
		f, err := os.OpenFile(mnt+"/sealed", os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		want := expected()
		m, err := syscall.Mmap(int(f.Fd()), 0, 257*4096, syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			return err
		}
		defer syscall.Munmap(m)
		if !bytes.Equal(m[:len(want)], want) || !bytes.Equal(m[len(want):], make([]byte, 37)) {
			return fmt.Errorf("mmap data/tail differs")
		}
		if err := errno(syscall.Mprotect(m, syscall.PROT_READ|syscall.PROT_WRITE), syscall.EACCES); err != nil {
			return err
		}
		if _, err := f.WriteAt([]byte("bad"), 0); !errors.Is(err, syscall.EPERM) {
			return fmt.Errorf("sealed write: %v", err)
		}
		if err := errno(f.Truncate(1), syscall.EPERM); err != nil {
			return err
		}
		w, err := syscall.Mmap(int(f.Fd()), 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err == nil {
			syscall.Munmap(w)
			return fmt.Errorf("writable shared mmap accepted")
		}
		priv, err := syscall.Mmap(int(f.Fd()), 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE)
		if err != nil {
			return err
		}
		priv[0] ^= 0xff
		syscall.Munmap(priv)
		got := make([]byte, 4096)
		if _, err := f.ReadAt(got, 0); err != nil {
			return err
		}
		if !bytes.Equal(got, want[:4096]) {
			return fmt.Errorf("private write reached data")
		}
		return nil
	})
	check("mutable file seals after fsync", func() error {
		f, err := os.OpenFile(mnt+"/growing", os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		want := bytes.Repeat([]byte("x"), 3*4096)
		if _, err := f.WriteAt(want, 0); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0600); err != nil {
			return err
		}
		got := make([]byte, len(want))
		if _, err := f.ReadAt(got, 0); err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("sealed generation differs")
		}
		_, err = f.WriteAt([]byte("no"), 0)
		return errno(err, syscall.EPERM)
	})
	check("directory sync and callback errors", func() error {
		d, err := os.Open(mnt)
		if err != nil {
			return err
		}
		defer d.Close()
		if err := d.Sync(); err != nil {
			return err
		}
		f, err := os.OpenFile(mnt+"/sync-fail", os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		return errno(f.Sync(), syscall.EIO)
	})
	check("reject invalid extent descriptors", func() error {
		for _, name := range []string{"bad-zero", "bad-device", "bad-overlap"} {
			f, err := os.Open(mnt + "/" + name)
			if err == nil {
				f.Close()
				return fmt.Errorf("accepted %s", name)
			}
			if err := errno(err, syscall.EINVAL); err != nil {
				return err
			}
		}
		return nil
	})
	check("native reread after inode eviction", func() error {
		if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0600); err != nil {
			return err
		}
		f, err := os.Open(mnt + "/sealed")
		if err != nil {
			return err
		}
		defer f.Close()
		got, err := io.ReadAll(f)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, expected()) {
			return fmt.Errorf("reread differs")
		}
		return nil
	})
	check("umount", func() error { return syscall.Unmount(mnt, 0) })
	if failed {
		os.Exit(1)
	}
	fmt.Println("fsblocktest: PASS")
}
