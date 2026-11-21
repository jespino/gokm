// Command brdtest checks gobrd inside the QEMU test VM (test/qemu/run.sh,
// with gobrd loaded as "rd_nr=2 rd_size=40960"). Linux's own brd is the
// oracle: the same random reads, writes and discards go to /dev/ram0 and
// /dev/gobrd0, and must give the same data. Then: concurrent O_DIRECT I/O,
// partitions, memory released by discard, and Linux's vfat on gobrd.
package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	sizeKiB = 40960
	size    = sizeKiB << 10
	gobrd0  = "/dev/gobrd0"
	gobrd1  = "/dev/gobrd1"
	ram0    = "/dev/ram0"
	mnt     = "/mnt"

	blkDiscard    = 0x1277
	blkRRPart     = 0x125f
	blkGetSize64  = 0x80081272
	blkSSZGet     = 0x1268
	blkPBSZGet    = 0x127b
	sysFinitMod   = 313
	finitCompress = 4
)

var failed bool

func check(name string, err error) {
	if err != nil {
		fmt.Printf("brdtest: FAIL %s: %v\n", name, err)
		failed = true
		return
	}
	fmt.Printf("brdtest: ok   %s\n", name)
}

func main() {
	os.MkdirAll(mnt, 0o755)
	os.MkdirAll("/sys", 0o755)
	syscall.Mount("sysfs", "/sys", "sysfs", 0, "")
	check("parameters and sysfs", testSysfs())
	check("ioctls: size, block sizes", testIoctls())
	check("unwritten sectors read as zeros", testZeros())
	check("load brd (oracle)", loadModule("/extra/brd.ko.zst", "rd_nr=1 rd_size="+strconv.Itoa(sizeKiB)+" max_part=0"))
	check("random I/O identical to brd", testOracle())
	check("unaligned discards keep partial pages (as brd)", testPartialDiscard())
	check("concurrent O_DIRECT writers and readers", testConcurrent())
	check("discard gives back memory", testDiscardMemory())
	check("partitions", testPartitions())
	check("load fat, vfat", loadAll("/extra/nls_iso8859-1.ko.zst", "/extra/fat.ko.zst", "/extra/vfat.ko.zst"))
	check("Linux vfat on gobrd", testVfat())
	if failed {
		fmt.Println("brdtest: FAILED")
		os.Exit(1)
	}
	fmt.Println("brdtest: PASS")
}

func readFile(p string) string {
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

func expect(what, got, want string) error {
	if got != want {
		return fmt.Errorf("%s = %q, want %q", what, got, want)
	}
	return nil
}

func testSysfs() error {
	checks := [][3]string{
		{"rd_nr", readFile("/sys/module/gobrd/parameters/rd_nr"), "2"},
		{"rd_size", readFile("/sys/module/gobrd/parameters/rd_size"), strconv.Itoa(sizeKiB)},
		{"gobrd0 size", readFile("/sys/block/gobrd0/size"), strconv.Itoa(size / 512)},
		{"gobrd1 size", readFile("/sys/block/gobrd1/size"), strconv.Itoa(size / 512)},
		{"gobrd0 ro", readFile("/sys/block/gobrd0/ro"), "0"},
		{"rotational", readFile("/sys/block/gobrd0/queue/rotational"), "0"},
		{"discard_granularity", readFile("/sys/block/gobrd0/queue/discard_granularity"), "4096"},
	}
	for _, c := range checks {
		if err := expect(c[0], c[1], c[2]); err != nil {
			return err
		}
	}
	// One hardware queue per CPU.
	queues, _ := filepath.Glob("/sys/block/gobrd0/mq/*")
	cpus, _ := filepath.Glob("/sys/devices/system/cpu/cpu[0-9]*")
	if len(queues) != len(cpus) || len(queues) < 2 {
		return fmt.Errorf("%d hardware queues for %d CPUs", len(queues), len(cpus))
	}
	if _, err := os.Stat("/sys/block/gobrd2"); err == nil {
		return fmt.Errorf("gobrd2 exists with rd_nr=2")
	}
	return nil
}

func ioctlInt(f *os.File, req uintptr, out unsafe.Pointer) error {
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(out))
	if e != 0 {
		return e
	}
	return nil
}

func testIoctls() error {
	f, err := os.Open(gobrd0)
	if err != nil {
		return err
	}
	defer f.Close()
	var sz uint64
	var lbs, pbs int32
	if err := ioctlInt(f, blkGetSize64, unsafe.Pointer(&sz)); err != nil {
		return err
	}
	if err := ioctlInt(f, blkSSZGet, unsafe.Pointer(&lbs)); err != nil {
		return err
	}
	if err := ioctlInt(f, blkPBSZGet, unsafe.Pointer(&pbs)); err != nil {
		return err
	}
	if sz != size || lbs != 512 || pbs != 4096 {
		return fmt.Errorf("size %d, logical %d, physical %d", sz, lbs, pbs)
	}
	return nil
}

// alignedBuf returns an n-byte buffer aligned for O_DIRECT.
func alignedBuf(n int) []byte {
	b := make([]byte, n+4096)
	off := int(-uintptr(unsafe.Pointer(&b[0])) & 4095)
	return b[off : off+n]
}

func openDirect(dev string) (*os.File, error) {
	return os.OpenFile(dev, os.O_RDWR|syscall.O_DIRECT, 0)
}

func testZeros() error {
	f, err := openDirect(gobrd0)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := alignedBuf(1 << 20)
	for _, off := range []int64{0, 4096 * 7, size/2 + 512, size - 1<<20} {
		for i := range buf {
			buf[i] = 0xaa
		}
		if _, err := f.ReadAt(buf, off); err != nil {
			return err
		}
		if !bytes.Equal(buf, make([]byte, len(buf))) {
			return fmt.Errorf("non-zero data at %d", off)
		}
	}
	// Reading past the end: EOF.
	if n, _ := f.ReadAt(buf[:4096], size); n != 0 {
		return fmt.Errorf("read %d bytes past the end", n)
	}
	return nil
}

func loadModule(path, params string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	p := append([]byte(params), 0)
	_, _, e := syscall.Syscall(sysFinitMod, f.Fd(), uintptr(unsafe.Pointer(&p[0])), finitCompress)
	if e != 0 {
		return fmt.Errorf("finit_module %s: %v", path, e)
	}
	return nil
}

func loadAll(paths ...string) error {
	for _, p := range paths {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			continue
		}
		if err := loadModule(p, ""); err != nil {
			return err
		}
	}
	return nil
}

func discard(f *os.File, off, n uint64) error {
	r := [2]uint64{off, n}
	return ioctlInt(f, blkDiscard, unsafe.Pointer(&r))
}

func devSum(dev string) ([32]byte, error) {
	f, err := openDirect(dev)
	if err != nil {
		return [32]byte{}, err
	}
	defer f.Close()
	h := sha256.New()
	buf := alignedBuf(1 << 20)
	for off := int64(0); off < size; off += int64(len(buf)) {
		if _, err := f.ReadAt(buf, off); err != nil {
			return [32]byte{}, err
		}
		h.Write(buf)
	}
	var s [32]byte
	copy(s[:], h.Sum(nil))
	return s, nil
}

// testOracle applies the same random operations to brd and gobrd.
func testOracle() error {
	var buf, dir [2]*os.File
	for i, dev := range []string{ram0, gobrd0} {
		var err error
		if buf[i], err = os.OpenFile(dev, os.O_RDWR, 0); err != nil {
			return err
		}
		defer buf[i].Close()
		if dir[i], err = openDirect(dev); err != nil {
			return err
		}
		defer dir[i].Close()
	}
	rng := rand.New(rand.NewSource(1))
	data := alignedBuf(1 << 20)
	got := [2][]byte{alignedBuf(1 << 20), alignedBuf(1 << 20)}
	counts := map[string]int{}
	log = nil
	for op := 0; op < 3000; op++ {
		kind := rng.Intn(10)
		switch {
		case kind < 3: // buffered write, any offset and length
			n := 1 + rng.Intn(200<<10)
			off := rng.Int63n(size - int64(n))
			rng.Read(data[:n])
			record(op, "bw", off, int64(n))
			for i := range buf {
				if _, err := buf[i].WriteAt(data[:n], off); err != nil {
					return fmt.Errorf("op %d: buffered write: %v", op, err)
				}
			}
			counts["buffered write"]++
		case kind < 6: // O_DIRECT write, sector aligned
			n := 512 * (1 + rng.Intn(len(data)/512))
			off := 512 * rng.Int63n((size-int64(n))/512+1)
			rng.Read(data[:n])
			record(op, "dw", off, int64(n))
			for i := range dir {
				if _, err := dir[i].WriteAt(data[:n], off); err != nil {
					return fmt.Errorf("op %d: direct write: %v", op, err)
				}
			}
			counts["direct write"]++
		case kind < 9: // read both ways, compare
			direct := kind == 8
			n := 1 + rng.Intn(len(data))
			off := rng.Int63n(size - int64(n))
			fs := buf
			if direct {
				n = 512 * (1 + rng.Intn(len(data)/512))
				off = 512 * rng.Int63n((size-int64(n))/512+1)
				fs = dir
			}
			for i := range fs {
				if _, err := fs[i].ReadAt(got[i][:n], off); err != nil {
					return fmt.Errorf("op %d: read: %v", op, err)
				}
			}
			if !bytes.Equal(got[0][:n], got[1][:n]) {
				first, last := -1, -1
				for j := 0; j < n; j++ {
					if got[0][j] != got[1][j] {
						if first < 0 {
							first = j
						}
						last = j
					}
				}
				zero := bytes.Equal(got[1][first:last+1], make([]byte, last+1-first))
				zeroBrd := bytes.Equal(got[0][first:last+1], make([]byte, last+1-first))
				return fmt.Errorf("op %d: %d bytes at %d (direct %v) differ from brd: device bytes [%d, %d], gobrd zero %v, brd zero %v; history: %s",
					op, n, off, direct, off+int64(first), off+int64(last), zero, zeroBrd, history(off+int64(first), off+int64(last)+1))
			}
			counts["read"]++
		default: // discard, page aligned (see testPartialDiscard)
			n := 4096 * (1 + uint64(rng.Intn(512)))
			off := 4096 * uint64(rng.Int63n((size-int64(n))/4096+1))
			record(op, "discard", int64(off), int64(n))
			for i := range dir {
				buf[i].Sync()
				if err := discard(dir[i], off, n); err != nil {
					return fmt.Errorf("op %d: discard: %v", op, err)
				}
			}
			counts["discard"]++
		}
		if op%500 == 499 {
			for i := range buf {
				buf[i].Sync()
			}
			a, err := devSum(ram0)
			if err != nil {
				return err
			}
			b, err := devSum(gobrd0)
			if err != nil {
				return err
			}
			if a != b {
				return fmt.Errorf("after op %d: device contents differ from brd", op)
			}
		}
	}
	fmt.Printf("brdtest:      ops: %v; whole-device SHA-256 equal every 500 ops\n", counts)
	return nil
}

type logEntry struct {
	op      int
	kind    string
	off, n  int64
}

var log []logEntry

func record(op int, kind string, off, n int64) { log = append(log, logEntry{op, kind, off, n}) }

// history lists the operations that touched [from, to).
func history(from, to int64) string {
	var parts []string
	for _, e := range log {
		if e.off < to && e.off+e.n > from {
			parts = append(parts, fmt.Sprintf("%d:%s[%d+%d]", e.op, e.kind, e.off, e.n))
		}
	}
	return strings.Join(parts, " ")
}

// testPartialDiscard discards unaligned ranges on both devices and compares
// them with O_DIRECT reads. Buffered reads aren't comparable here: the
// discard ioctl truncates the page cache, which zeroes the discarded part of
// a cached partial page in the cache only, and what's cached differs between
// the devices (readahead).
func testPartialDiscard() error {
	var dir [2]*os.File
	for i, dev := range []string{ram0, gobrd0} {
		var err error
		if dir[i], err = openDirect(dev); err != nil {
			return err
		}
		defer dir[i].Close()
	}
	const base, span = 1 << 20, 64 << 10
	data := alignedBuf(span)
	for i := range data {
		data[i] = byte(i*7 + 1)
	}
	rng := rand.New(rand.NewSource(3))
	got := [2][]byte{alignedBuf(span), alignedBuf(span)}
	for round := 0; round < 50; round++ {
		for i := range dir {
			if _, err := dir[i].WriteAt(data, base); err != nil {
				return err
			}
		}
		off := 512 * uint64(rng.Intn(span/512-1))
		n := 512 * (1 + uint64(rng.Intn(int(span-off)/512)))
		for i := range dir {
			if err := discard(dir[i], base+off, n); err != nil {
				return err
			}
			if _, err := dir[i].ReadAt(got[i], base); err != nil {
				return err
			}
		}
		if !bytes.Equal(got[0], got[1]) {
			return fmt.Errorf("discard [%d, +%d): contents differ from brd", off, n)
		}
		// Whole pages inside the range read as zeros, the rest is intact.
		for j := uint64(0); j < span; j += 512 {
			inPage := (j/4096)*4096 >= off && (j/4096+1)*4096 <= off+n
			want := data[j : j+512]
			if inPage {
				want = make([]byte, 512)
			}
			if !bytes.Equal(got[1][j:j+512], want) {
				return fmt.Errorf("discard [%d, +%d): wrong data at %d", off, n, j)
			}
		}
	}
	return nil
}

// testConcurrent runs 8 O_DIRECT writers on separate regions of gobrd1 plus
// 4 readers, then checks every region.
func testConcurrent() error {
	const writers, region = 8, 4 << 20
	want := make([][]byte, writers)
	stop := make(chan struct{})
	var wg, rg sync.WaitGroup
	errs := make(chan error, writers+4)
	start := time.Now()
	for r := 0; r < 4; r++ {
		rg.Add(1)
		go func(r int) {
			defer rg.Done()
			f, err := openDirect(gobrd1)
			if err != nil {
				errs <- err
				return
			}
			defer f.Close()
			rng := rand.New(rand.NewSource(int64(100 + r)))
			b := alignedBuf(128 << 10)
			for {
				select {
				case <-stop:
					return
				default:
				}
				off := 512 * rng.Int63n((size-int64(len(b)))/512)
				if _, err := f.ReadAt(b, off); err != nil {
					errs <- err
					return
				}
			}
		}(r)
	}
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			f, err := openDirect(gobrd1)
			if err != nil {
				errs <- err
				return
			}
			defer f.Close()
			rng := rand.New(rand.NewSource(int64(w)))
			mine := make([]byte, region)
			b := alignedBuf(256 << 10)
			base := int64(w) * region
			for i := 0; i < 400; i++ {
				n := 512 * (1 + rng.Intn(len(b)/512))
				off := 512 * rng.Int63n(int64(region-n)/512+1)
				rng.Read(b[:n])
				if _, err := f.WriteAt(b[:n], base+off); err != nil {
					errs <- err
					return
				}
				copy(mine[off:], b[:n])
			}
			want[w] = mine
		}(w)
	}
	wg.Wait()
	close(stop)
	rg.Wait()
	close(errs)
	for err := range errs {
		return err
	}
	elapsed := time.Since(start)
	f, err := openDirect(gobrd1)
	if err != nil {
		return err
	}
	defer f.Close()
	b := alignedBuf(region)
	for w := 0; w < writers; w++ {
		if _, err := f.ReadAt(b, int64(w)*region); err != nil {
			return err
		}
		if !bytes.Equal(b, want[w]) {
			return fmt.Errorf("region %d corrupted", w)
		}
	}
	fmt.Printf("brdtest:      %d writers x 400 writes + 4 readers in %v\n", writers, elapsed.Round(time.Millisecond))
	return nil
}

func memFree() int64 {
	data, _ := os.ReadFile("/proc/meminfo")
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemFree:" {
			n, _ := strconv.ParseInt(f[1], 10, 64)
			return n << 10
		}
	}
	return 0
}

// testDiscardMemory fills gobrd1, discards it all and checks the memory
// came back, and that it reads as zeros.
func testDiscardMemory() error {
	f, err := openDirect(gobrd1)
	if err != nil {
		return err
	}
	defer f.Close()
	b := alignedBuf(1 << 20)
	for i := range b {
		b[i] = byte(i)
	}
	for off := int64(0); off < size; off += int64(len(b)) {
		if _, err := f.WriteAt(b, off); err != nil {
			return err
		}
	}
	before := memFree()
	if err := discard(f, 0, size); err != nil {
		return err
	}
	after := memFree()
	if after-before < size/2 {
		return fmt.Errorf("MemFree grew by %d KiB after discarding %d KiB", (after-before)>>10, size>>10)
	}
	for _, off := range []int64{0, size / 2, size - int64(len(b))} {
		if _, err := f.ReadAt(b, off); err != nil {
			return err
		}
		if !bytes.Equal(b, make([]byte, len(b))) {
			return fmt.Errorf("discarded data at %d isn't zero", off)
		}
	}
	fmt.Printf("brdtest:      MemFree +%d KiB after discarding %d KiB\n", (after-before)>>10, size>>10)
	return nil
}

// testPartitions writes an MBR with two partitions to gobrd1, rescans and
// does I/O through them.
func testPartitions() error {
	f, err := os.OpenFile(gobrd1, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	mbr := make([]byte, 512)
	parts := [][2]uint32{{2048, 16384}, {18432, 40960}} // start, sectors
	for i, p := range parts {
		e := mbr[446+16*i:]
		e[4] = 0x83
		putLE32(e[8:], p[0])
		putLE32(e[12:], p[1])
	}
	mbr[510], mbr[511] = 0x55, 0xaa
	if _, err := f.WriteAt(mbr, 0); err != nil {
		f.Close()
		return err
	}
	f.Sync()
	err = ioctlInt(f, blkRRPart, nil)
	f.Close()
	if err != nil {
		return fmt.Errorf("BLKRRPART: %v", err)
	}
	for i, p := range parts {
		name := fmt.Sprintf("gobrd1p%d", i+1)
		if err := expect(name+" size", readFile(filepath.Join("/sys/block/gobrd1", name, "size")), strconv.Itoa(int(p[1]))); err != nil {
			return err
		}
		var dev string
		for t := 0; t < 50; t++ {
			if _, err := os.Stat("/dev/" + name); err == nil {
				dev = "/dev/" + name
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if dev == "" {
			return fmt.Errorf("/dev/%s didn't appear", name)
		}
		pf, err := openDirect(dev)
		if err != nil {
			return err
		}
		b := alignedBuf(8192)
		copy(b, "partition "+name)
		_, err = pf.WriteAt(b, 4096)
		pf.Close()
		if err != nil {
			return err
		}
		// Seen through the whole disk at the partition's offset.
		wf, err := openDirect(gobrd1)
		if err != nil {
			return err
		}
		got := alignedBuf(8192)
		_, err = wf.ReadAt(got, int64(p[0])*512+4096)
		wf.Close()
		if err != nil {
			return err
		}
		if !bytes.Equal(got, b) {
			return fmt.Errorf("%s: data not at the partition's offset", name)
		}
	}
	return nil
}

func putLE32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

// testVfat formats gobrd0 as FAT32 and uses it with Linux's vfat, across a
// remount.
func testVfat() error {
	if err := mkfs(gobrd0, 1); err != nil {
		return fmt.Errorf("mkfs: %v", err)
	}
	if err := syscall.Mount(gobrd0, mnt, "vfat", 0, ""); err != nil {
		return fmt.Errorf("mount: %v", err)
	}
	rng := rand.New(rand.NewSource(7))
	files := map[string][32]byte{}
	for d := 0; d < 4; d++ {
		dir := filepath.Join(mnt, fmt.Sprintf("dir %d with a long name", d))
		if err := os.Mkdir(dir, 0o755); err != nil {
			return err
		}
		for i := 0; i < 25; i++ {
			data := make([]byte, rng.Intn(600<<10))
			rng.Read(data)
			p := filepath.Join(dir, fmt.Sprintf("file-%03d.bin", i))
			if err := os.WriteFile(p, data, 0o644); err != nil {
				return err
			}
			files[p] = sha256.Sum256(data)
		}
	}
	if err := syscall.Unmount(mnt, 0); err != nil {
		return fmt.Errorf("umount: %v", err)
	}
	// Drop the page cache so the re-read comes from gobrd.
	os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0)
	if err := syscall.Mount(gobrd0, mnt, "vfat", syscall.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("remount: %v", err)
	}
	defer syscall.Unmount(mnt, 0)
	for p, sum := range files {
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if sha256.Sum256(data) != sum {
			return fmt.Errorf("%s: content differs after remount", p)
		}
	}
	var st syscall.Statfs_t
	syscall.Statfs(mnt, &st)
	fmt.Printf("brdtest:      %d files verified after remount; %d of %d blocks used\n",
		len(files), st.Blocks-st.Bfree, st.Blocks)
	return nil
}
