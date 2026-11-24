package build

import (
	"bufio"
	"debug/elf"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/jespino/gokm/internal/assets"
	"github.com/jespino/tinyguest/project"
)

// rtCFlags compile compiler-rt's builtins for the kernel: the kernel codegen
// flags plus the soft-float ABI our Go code uses (target.json has
// +soft-float), so float/double values travel in integer registers and no
// SSE/x87 instruction is ever emitted.
var rtCFlags = append(append([]string{}, kernelCFlags...),
	"-msoft-float", "-mno-sse", "-mno-mmx", "-mno-80387",
	"-ffreestanding", "-fno-builtin", "-fno-stack-protector",
	"-fno-asynchronous-unwind-tables", "-fno-unwind-tables",
)

// rtLib is compiler-rt's builtins library (soft-float arithmetic, 128-bit
// division, ...) compiled for the kernel, one object per source file. The
// kernel exports none of these, but TinyGo's code calls them (e.g. float
// comparisons in interface equality), so the objects a module needs are
// linked into its Go object.
type rtLib struct {
	Objects map[string]*rtObject // by object path
	defs    map[string]string    // symbol -> object path
}

type rtObject struct {
	Defined   []string
	Undefined []string
}

func (b *builder) prepareRTLib(root *assets.Root, cacheDir, tinygoVersion string) (*rtLib, error) {
	// Depends only on the TinyGo install and the flags, not gokm's assets.
	dir := filepath.Join(cacheDir, "rtlib-"+project.ShortHash(tinygoVersion+"\x00"+strings.Join(rtCFlags, " ")))
	index := filepath.Join(dir, "index.json")
	lib := &rtLib{}
	if data, err := os.ReadFile(index); err == nil && json.Unmarshal(data, &lib.Objects) == nil {
		lib.index()
		return lib, nil
	}

	b.Step("compiling compiler-rt builtins for the kernel (once per TinyGo install)")
	src, include := rtSourceDirs(root.TinyGoRoot)
	files, err := filepath.Glob(filepath.Join(src, "*.c"))
	if err != nil || len(files) == 0 {
		return nil, fmt.Errorf("compiler-rt builtins not found in %s", src)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	lib.Objects = map[string]*rtObject{}
	var mu sync.Mutex
	work := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < runtime.NumCPU(); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range work {
				obj := filepath.Join(dir, strings.TrimSuffix(filepath.Base(f), ".c")+".o")
				args := append([]string{"clang"}, rtCFlags...)
				args = append(args, "-isystem", include, "-c", f, "-o", obj)
				if exec.Command("tinygo", args...).Run() != nil {
					continue // not buildable for this target (arch-specific etc.)
				}
				o, err := readSymbols(obj)
				if err != nil || len(o.Defined) == 0 {
					continue
				}
				mu.Lock()
				lib.Objects[obj] = o
				mu.Unlock()
			}
		}()
	}
	for _, f := range files {
		work <- f
	}
	close(work)
	wg.Wait()

	data, err := json.Marshal(lib.Objects)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(index, data, 0o644); err != nil {
		return nil, err
	}
	lib.index()
	return lib, nil
}

// rtSourceDirs returns the compiler-rt builtins sources and clang headers of
// a TinyGo release, or of a TinyGo source checkout built with `make`.
func rtSourceDirs(tinygoRoot string) (src, include string) {
	src = filepath.Join(tinygoRoot, "lib", "compiler-rt-builtins")
	include = filepath.Join(tinygoRoot, "lib", "clang", "include")
	if _, err := os.Stat(src); err != nil {
		src = filepath.Join(tinygoRoot, "llvm-project", "compiler-rt", "lib", "builtins")
	}
	if _, err := os.Stat(include); err != nil {
		if dirs, _ := filepath.Glob(filepath.Join(tinygoRoot, "llvm-build", "lib", "clang", "*", "include")); len(dirs) > 0 {
			include = dirs[len(dirs)-1]
		}
	}
	return src, include
}

func (l *rtLib) index() {
	l.defs = map[string]string{}
	for path, o := range l.Objects {
		for _, s := range o.Defined {
			l.defs[s] = path
		}
	}
}

// resolve returns the objects needed to define undefined (transitively),
// skipping symbols the kernel exports, and the global symbols they define.
func (l *rtLib) resolve(undefined []string, kernelExports map[string]bool) (objs, defined []string) {
	selected := map[string]bool{}
	queue := append([]string{}, undefined...)
	for len(queue) > 0 {
		sym := queue[0]
		queue = queue[1:]
		if kernelExports[sym] {
			continue
		}
		path, ok := l.defs[sym]
		if !ok || selected[path] {
			continue
		}
		selected[path] = true
		objs = append(objs, path)
		defined = append(defined, l.Objects[path].Defined...)
		queue = append(queue, l.Objects[path].Undefined...)
	}
	sort.Strings(objs)
	return objs, defined
}

// readSymbols lists an ELF object's defined global and undefined symbols.
func readSymbols(path string) (*rtObject, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	syms, err := f.Symbols()
	if err != nil {
		return nil, err
	}
	o := &rtObject{}
	for _, s := range syms {
		if s.Name == "" {
			continue
		}
		bind := elf.ST_BIND(s.Info)
		switch {
		case s.Section == elf.SHN_UNDEF:
			o.Undefined = append(o.Undefined, s.Name)
		case bind == elf.STB_GLOBAL || bind == elf.STB_WEAK:
			o.Defined = append(o.Defined, s.Name)
		}
	}
	return o, nil
}

// readKernelExports reads the symbols exported by vmlinux from a kernel build
// dir's Module.symvers (symbol -> namespace, "" for none).
func readKernelExports(kdir string) (map[string]string, error) {
	f, err := os.Open(filepath.Join(kdir, "Module.symvers"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	exports := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// crc \t symbol \t module \t export-type \t namespace
		fields := strings.Split(sc.Text(), "\t")
		if len(fields) < 4 {
			continue
		}
		ns := ""
		if len(fields) >= 5 {
			ns = fields[4]
		}
		exports[fields[1]] = ns
	}
	return exports, sc.Err()
}
