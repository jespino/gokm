// Package build turns a Go kernel module project into a .ko file.
//
// Pipeline:
//
//	project *.go ──tinygo build (custom target, overlay TINYGOROOT)──▶ go.ll
//	go.ll ──fixupIR──▶ go.fixed.ll ──tinygo clang (kernel codegen flags)──▶ go.raw.o
//	go.raw.o ──objcopy (strip debug, localize symbols)──▶ kmod/go.o_shipped
//	kmod/{shim.c, go.o_shipped} ──kbuild──▶ <name>.ko
package build

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/jespino/gokm/internal/assets"
	"github.com/jespino/gokm/internal/bindgen"
)

// Options configure a build.
type Options struct {
	Dir         string // project directory (contains go.mod)
	Output      string // output .ko path; default <Dir>/<Name>.ko
	Name        string // module name; default derived from the go.mod module path
	KernelVer   string // kernel release to build for; default `uname -r`
	KernelDir   string // kernel build dir; default /lib/modules/<KernelVer>/build
	Description string // MODULE_DESCRIPTION; default "<module path> (gokm)"
	Author      string // MODULE_AUTHOR; optional
	License     string // MODULE_LICENSE; one of Licenses (default "GPL")
	StackLimit  string // warn about call paths using more stack than this (default 8K; "0" disables)
	HeapSize    string // initial Go heap size (module parameter heap_size); default DefaultHeapSize
	HeapMax     string // maximum Go heap size (module parameter heap_max); default DefaultHeapMax
	Verbose     bool   // print commands and their full output
	Log         io.Writer
}

// Result describes a finished build.
type Result struct {
	Output    string
	Name      string
	KernelDir string
	WorkDir   string
}

// DefaultHeapSize is the default size of a module's garbage-collected Go heap.
const DefaultHeapSize = "1M"

// Licenses are the MODULE_LICENSE values gokm accepts: the GPL-compatible
// ones the kernel recognizes. gokm's runtime uses GPL-only kernel symbols,
// so non-GPL-compatible licenses can't work.
var Licenses = []string{"GPL", "GPL v2", "GPL and additional rights", "Dual BSD/GPL", "Dual MIT/GPL", "Dual MPL/GPL"}

// DefaultHeapMax is the default limit the Go heap can grow to.
const DefaultHeapMax = "256M"

var heapSizeRe = regexp.MustCompile(`^[0-9]+[KMG]?$`)

// kernel codegen flags for lowering TinyGo's IR. These mirror what kbuild uses
// for C on x86-64 and aren't exposed through TinyGo's target JSON.
var kernelCFlags = []string{
	"--target=x86_64-unknown-linux-gnu", "-O2",
	"-mcmodel=kernel", "-fno-pic", "-mno-red-zone",
	"-fcf-protection=branch",
	"-mretpoline-external-thunk", "-mfunction-return=thunk-extern",
}

// Build runs the full pipeline.
func Build(opts Options) (*Result, error) {
	if opts.Log == nil {
		opts.Log = os.Stderr
	}
	b := &builder{opts: opts}
	return b.run()
}

type builder struct {
	opts   Options
	params []moduleParam // //gokm:param variables of the project
}

func (b *builder) step(format string, args ...any) {
	fmt.Fprintf(b.opts.Log, "gokm: "+format+"\n", args...)
}

func (b *builder) run() (*Result, error) {
	proj, err := filepath.Abs(b.opts.Dir)
	if err != nil {
		return nil, err
	}
	mod, err := ReadGoMod(filepath.Join(proj, "go.mod"))
	if err != nil {
		return nil, err
	}
	name := b.opts.Name
	if name == "" {
		name = ModuleNameFromPath(mod.Path)
	}
	if err := ValidateModuleName(name); err != nil {
		return nil, err
	}
	if b.opts.HeapSize == "" {
		b.opts.HeapSize = DefaultHeapSize
	}
	if b.opts.License == "" {
		b.opts.License = "GPL"
	}
	if !slices.Contains(Licenses, b.opts.License) {
		return nil, fmt.Errorf("license %q not supported: use one of %q (gokm's runtime needs GPL-only kernel symbols)",
			b.opts.License, Licenses)
	}
	if b.opts.HeapMax == "" {
		b.opts.HeapMax = DefaultHeapMax
	}
	for _, v := range []string{b.opts.HeapSize, b.opts.HeapMax} {
		if !heapSizeRe.MatchString(v) {
			return nil, fmt.Errorf("invalid heap size %q: use bytes or a K/M/G suffix, e.g. 4M", v)
		}
	}

	kdir, err := KernelBuildDir(b.opts.KernelVer, b.opts.KernelDir)
	if err != nil {
		return nil, err
	}
	for _, tool := range []string{"tinygo", "objcopy", "ld", "make"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("%s not found in PATH", tool)
		}
	}

	cache, err := CacheDir()
	if err != nil {
		return nil, err
	}
	tgRoot, tgVersion, err := tinygoInfo()
	if err != nil {
		return nil, err
	}
	root, err := assets.Prepare(cache, tgRoot, tgVersion)
	if err != nil {
		return nil, err
	}

	work := filepath.Join(cache, "build", shortHash(proj))
	kmod := filepath.Join(work, "kmod")
	if err := os.MkdirAll(kmod, 0o755); err != nil {
		return nil, err
	}

	exports, err := readKernelExports(kdir)
	if err != nil {
		return nil, fmt.Errorf("reading kernel exports: %w", err)
	}

	// The project is built from a copy, with gokm pragmas rewritten.
	src := filepath.Join(work, "src")
	scan, err := prepareSource(proj, src)
	if scan != nil {
		b.params = scan.params
	}
	if err != nil {
		return nil, err
	}
	genRoot := filepath.Join(work, "gen")
	if err := os.RemoveAll(genRoot); err != nil {
		return nil, err
	}
	var gen *bindgen.Output
	var extraModules []string
	if len(scan.names) > 0 || len(scan.pragmas) > 0 {
		b.step("generating %s bindings (%d names, %d pragmas)", LinuxPkg, len(scan.names), len(scan.pragmas))
		genDir := filepath.Join(genRoot, "linux")
		if gen, err = b.generateBindings(scan, kdir, work, genDir, exports); err != nil {
			return nil, err
		}
		extraModules = append(extraModules, genDir)
	}

	// A workspace that makes gokm's packages (and gokm/linux) resolvable.
	goWork := filepath.Join(work, "go.work")
	if err := os.WriteFile(goWork, []byte(GoWorkContent(mod.GoVersion, src, root.Lib, false, extraModules...)), 0o644); err != nil {
		return nil, err
	}
	RefreshProjectGoWork(proj, mod.GoVersion, root.Lib, extraModules...)

	// 1. Go -> LLVM IR.
	b.step("compiling Go code with TinyGo %s", shortTinyGoVersion(tgVersion))
	ll := filepath.Join(work, "go.ll")
	tgEnv := append(os.Environ(), "TINYGOROOT="+root.TinyGoRoot, "GOWORK="+goWork)
	cmd := exec.Command("tinygo", "build", "-target="+root.Target, "-opt=2", "-no-debug", "-o", ll, ".")
	cmd.Dir = src
	cmd.Env = tgEnv
	if err := b.exec(cmd); err != nil {
		// Report positions in the project, not in the copy.
		return nil, errors.New(strings.ReplaceAll(err.Error(), src+string(filepath.Separator), proj+string(filepath.Separator)))
	}
	frameworks, err := b.frameworks(root, src, tgEnv)
	if err != nil {
		return nil, err
	}

	// 2. Patch the IR for the kernel ABI.
	raw, err := os.ReadFile(ll)
	if err != nil {
		return nil, err
	}
	fixed := filepath.Join(work, "go.fixed.ll")
	if err := os.WriteFile(fixed, fixupIR(raw), 0o644); err != nil {
		return nil, err
	}

	// 3. IR -> object with kernel codegen flags.
	rawObj := filepath.Join(work, "go.raw.o")
	args := append([]string{"clang"}, kernelCFlags...)
	args = append(args, "-c", "-x", "ir", fixed, "-o", rawObj)
	if err := b.exec(exec.Command("tinygo", args...)); err != nil {
		return nil, err
	}

	// Stack-usage analysis, on a separate analysis build of the object, in
	// parallel with the rest of the pipeline.
	type stackResult struct {
		entries []*stackEntry
		err     error
	}
	stackDone := make(chan stackResult, 1)
	if b.opts.StackLimit != "0" {
		go func() {
			obj := filepath.Join(work, "go.stack.o")
			args := append([]string{"clang"}, kernelCFlags...)
			args = append(args, "-fstack-size-section", "-ffunction-sections", "-c", "-x", "ir", fixed, "-o", obj)
			if out, err := exec.Command("tinygo", args...).CombinedOutput(); err != nil {
				stackDone <- stackResult{err: fmt.Errorf("%v: %s", err, out)}
				return
			}
			g, err := analyzeStack(obj, fixed, mod.Path)
			if err != nil {
				stackDone <- stackResult{err: err}
				return
			}
			stackDone <- stackResult{entries: g.entries()}
		}()
	} else {
		close(stackDone)
	}

	// 4. Link in the compiler-rt builtins the Go code needs (soft-float,
	// 128-bit arithmetic): the kernel doesn't provide them.
	rt, err := b.prepareRTLib(root, cache, tgVersion)
	if err != nil {
		return nil, err
	}
	kernelExports := make(map[string]bool, len(exports))
	for sym := range exports {
		kernelExports[sym] = true
	}
	goSyms, err := readSymbols(rawObj)
	if err != nil {
		return nil, err
	}
	rtObjs, rtDefined := rt.resolve(goSyms.Undefined, kernelExports)
	linkedObj := rawObj
	if len(rtObjs) > 0 {
		linkedObj = filepath.Join(work, "go.linked.o")
		args := append([]string{"-r", "-o", linkedObj, rawObj}, rtObjs...)
		if err := b.exec(exec.Command("ld", args...)); err != nil {
			return nil, err
		}
	}

	// 5. Strip debug info (pahole can't turn Go DWARF names like "[]byte"
	// into valid BTF) and make Go internals, TinyGo's libc/atomic helpers and
	// the builtins local. What stays global: the runtime entry points shim.c
	// calls and //export functions, which C glue may call.
	localize := filepath.Join(work, "localize.txt")
	syms := append(append([]string{}, localizedSymbols...), rtDefined...)
	if err := os.WriteFile(localize, []byte(strings.Join(syms, "\n")+"\n"), 0o644); err != nil {
		return nil, err
	}
	if err := b.exec(exec.Command("objcopy", "--strip-debug", "--wildcard",
		"--localize-symbols="+localize, linkedObj, filepath.Join(kmod, "go.o_shipped"))); err != nil {
		return nil, err
	}

	// 6. kbuild.
	if err := b.writeKmod(root, proj, kmod, name, mod.Path, frameworks, gen, genRoot); err != nil {
		return nil, err
	}
	b.step("building %s.ko against %s", name, kdir)
	if err := b.exec(exec.Command("make", "-C", kdir, "M="+kmod, "modules")); err != nil {
		return nil, err
	}

	if r, ok := <-stackDone; ok {
		b.reportStack(r.entries, r.err)
	}

	out := b.opts.Output
	if out == "" {
		out = filepath.Join(proj, name+".ko")
	}
	if err := copyFile(filepath.Join(kmod, name+".ko"), out); err != nil {
		return nil, err
	}
	return &Result{Output: out, Name: name, KernelDir: kdir, WorkDir: work}, nil
}

// CSrcDir is the project subdirectory whose .c/.h files are compiled into the
// module (it can't be the Go package directory: Go rejects .c files there
// without cgo).
const CSrcDir = "csrc"

// localizedSymbols are objcopy wildcard patterns for global symbols in the Go
// object that must not be visible to the rest of the module.
var localizedSymbols = []string{
	"*.*", // Go-internal symbols: runtime.alloc, main.main, ...
	"__atomic_*", "__sync_*", "__errno_location",
	"malloc", "calloc", "realloc", "free",
	"fmaximum*", "fminimum*", "runtime_putchar",
}

// copyCSources copies the .c/.h files of srcDir (if it exists) into
// kmod/sub and returns the kbuild objects for the .c files.
func copyCSources(srcDir, kmod, sub string) ([]string, error) {
	entries, err := os.ReadDir(srcDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(kmod, sub), 0o755); err != nil {
		return nil, err
	}
	var objs []string
	for _, e := range entries {
		n := e.Name()
		ext := filepath.Ext(n)
		if e.IsDir() || (ext != ".c" && ext != ".h") {
			continue
		}
		if err := copyFile(filepath.Join(srcDir, n), filepath.Join(kmod, sub, n)); err != nil {
			return nil, err
		}
		if ext == ".c" {
			objs = append(objs, sub+"/"+strings.TrimSuffix(n, ".c")+".o")
		}
	}
	return objs, nil
}

// frameworks returns the gokm framework packages (those with C glue) that
// the project imports, directly or not.
func (b *builder) frameworks(root *assets.Root, dir string, env []string) ([]string, error) {
	cmd := exec.Command("tinygo", "list", "-target="+root.Target, "-deps", ".")
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = ee.Stderr
		}
		return nil, fmt.Errorf("listing dependencies: %w\n%s", err, strings.TrimSpace(string(stderr)))
	}
	var fws []string
	for _, line := range strings.Split(string(out), "\n") {
		pkg, ok := strings.CutPrefix(strings.TrimSpace(line), assets.LibModule+"/")
		if !ok {
			continue
		}
		if st, err := os.Stat(filepath.Join(root.Frameworks, pkg)); err == nil && st.IsDir() {
			fws = append(fws, pkg)
		}
	}
	sort.Strings(fws)
	return fws, nil
}

func (b *builder) reportStack(entries []*stackEntry, err error) {
	if err != nil {
		b.step("warning: stack analysis failed: %v", err)
		return
	}
	limit := parseSize(b.opts.StackLimit)
	if limit == 0 {
		limit = 8 << 10
	}
	if b.opts.Verbose {
		b.step("deepest Go call paths (of a 16 KiB kernel stack):")
		for i, e := range entries {
			if i == 5 {
				break
			}
			fmt.Fprintf(b.opts.Log, "    %s\n", e.describe())
		}
	}
	for _, e := range entries {
		if e.Bytes > limit || e.Recursive {
			b.step("warning: stack: %s", e.describe())
		}
	}
}

// parseSize parses sizes like 8192, 8K, 4M.
func parseSize(s string) uint64 {
	mult := uint64(1)
	switch {
	case strings.HasSuffix(s, "K"):
		mult, s = 1<<10, strings.TrimSuffix(s, "K")
	case strings.HasSuffix(s, "M"):
		mult, s = 1<<20, strings.TrimSuffix(s, "M")
	}
	var n uint64
	fmt.Sscanf(s, "%d", &n)
	return n * mult
}

func (b *builder) writeKmod(root *assets.Root, proj, kmod, name, modPath string, frameworks []string,
	gen *bindgen.Output, genRoot string) error {
	// Drop outputs of a previous build that used a different module name.
	if entries, err := os.ReadDir(kmod); err == nil {
		for _, e := range entries {
			if e.Name() != "go.o_shipped" {
				os.RemoveAll(filepath.Join(kmod, e.Name()))
			}
		}
	}
	// The C runtime: shim.c, threads.c, heap.c, gokm.h.
	var runtimeObjs []string
	entries, err := os.ReadDir(root.Kmod)
	if err != nil {
		return err
	}
	for _, e := range entries {
		n := e.Name()
		if err := copyFile(filepath.Join(root.Kmod, n), filepath.Join(kmod, n)); err != nil {
			return err
		}
		if strings.HasSuffix(n, ".c") {
			runtimeObjs = append(runtimeObjs, strings.TrimSuffix(n, ".c")+".o")
		}
	}
	sort.Strings(runtimeObjs)

	// C sources: framework glue for imported gokm packages, then the
	// project's own csrc/.
	var userObjs []string
	for _, fw := range frameworks {
		objs, err := copyCSources(filepath.Join(root.Frameworks, fw), kmod, "gokm_"+fw)
		if err != nil {
			return err
		}
		userObjs = append(userObjs, objs...)
	}
	if len(frameworks) > 0 {
		b.step("using frameworks: gokm/%s", strings.Join(frameworks, ", gokm/"))
	}
	if gen != nil && (len(gen.C) > 0 || len(gen.CStandalone) > 0) {
		cdir := filepath.Join(genRoot, "c")
		if err := os.MkdirAll(cdir, 0o755); err != nil {
			return err
		}
		for name, src := range map[string][]byte{"wrappers.c": gen.C, "byval.c": gen.CStandalone} {
			if len(src) == 0 {
				continue
			}
			if err := os.WriteFile(filepath.Join(cdir, name), src, 0o644); err != nil {
				return err
			}
		}
		objs, err := copyCSources(cdir, kmod, "gokm_gen")
		if err != nil {
			return err
		}
		userObjs = append(userObjs, objs...)
	}
	if len(b.params) > 0 {
		src, err := paramsC(b.params)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(kmod, "gokm_params.c"), src, 0o644); err != nil {
			return err
		}
		userObjs = append(userObjs, "gokm_params.o")
		var names []string
		for _, p := range b.params {
			names = append(names, p.Name)
		}
		b.step("module parameters: %s", strings.Join(names, ", "))
	}
	objs, err := copyCSources(filepath.Join(proj, CSrcDir), kmod, CSrcDir)
	if err != nil {
		return err
	}
	if len(objs) > 0 {
		b.step("adding C sources: %s", strings.Join(objs, " "))
	}
	userObjs = append(userObjs, objs...)

	kbuild := fmt.Sprintf(`obj-m := %[1]s.o
%[1]s-y := %[3]s go.o %[2]s

# Project C files (csrc/) can #include "gokm.h".
ccflags-y += -I$(src)

# go.o is produced by TinyGo and handed to kbuild prebuilt as go.o_shipped.
# if_changed records the .go.o.cmd file modpost expects for every object.
quiet_cmd_shipped = SHIPPED $@
      cmd_shipped = cat $< > $@

$(obj)/go.o: $(src)/go.o_shipped FORCE
	$(call if_changed,shipped)

targets += go.o
`, name, strings.Join(userObjs, " "), strings.Join(runtimeObjs, " "))

	desc := b.opts.Description
	if desc == "" {
		desc = modPath + " (Go kernel module built with gokm)"
	}
	var info strings.Builder
	info.WriteString("/* Generated by gokm. */\n")
	fmt.Fprintf(&info, "MODULE_LICENSE(%s);\n", cString(b.opts.License))
	fmt.Fprintf(&info, "MODULE_DESCRIPTION(%s);\n", cString(desc))
	if b.opts.Author != "" {
		fmt.Fprintf(&info, "MODULE_AUTHOR(%s);\n", cString(b.opts.Author))
	}
	fmt.Fprintf(&info, "#define GOKM_DEFAULT_HEAP_SIZE %s\n", cString(b.opts.HeapSize))
	fmt.Fprintf(&info, "#define GOKM_DEFAULT_HEAP_MAX %s\n", cString(b.opts.HeapMax))
	if gen != nil {
		for _, ns := range gen.Namespaces {
			fmt.Fprintf(&info, "MODULE_IMPORT_NS(%s);\n", cString(ns))
		}
	}

	if err := os.WriteFile(filepath.Join(kmod, "Kbuild"), []byte(kbuild), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(kmod, "modinfo.h"), []byte(info.String()), 0o644)
}

// exec runs cmd. Output is shown only in verbose mode or when it fails.
func (b *builder) exec(cmd *exec.Cmd) error {
	if b.opts.Verbose {
		fmt.Fprintf(b.opts.Log, "+ %s\n", strings.Join(cmd.Args, " "))
		cmd.Stdout = b.opts.Log
		cmd.Stderr = b.opts.Log
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s failed: %w", filepath.Base(cmd.Path), err)
		}
		return nil
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	cmd.Env = withCLocale(cmd.Env)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w\n%s", filepath.Base(cmd.Path), err, strings.TrimSpace(buf.String()))
	}
	return nil
}

func withCLocale(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	return append(env, "LC_ALL=C")
}

// GoMod is the subset of go.mod gokm cares about.
type GoMod struct {
	Path      string
	GoVersion string
}

var (
	moduleRe = regexp.MustCompile(`(?m)^module\s+"?([^\s"]+)"?`)
	goVerRe  = regexp.MustCompile(`(?m)^go\s+(\S+)`)
)

// ReadGoMod parses the module path and go version from a go.mod file.
func ReadGoMod(path string) (*GoMod, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s not found (run `gokm init` first)", path)
		}
		return nil, err
	}
	m := moduleRe.FindSubmatch(data)
	if m == nil {
		return nil, fmt.Errorf("%s: no module directive", path)
	}
	gm := &GoMod{Path: string(m[1]), GoVersion: assets.LibGoVersion}
	if v := goVerRe.FindSubmatch(data); v != nil {
		gm.GoVersion = string(v[1])
	}
	return gm, nil
}

// ModuleNameFromPath derives a kernel module name from a Go module path:
// last path element, with characters invalid in module names replaced by "_".
func ModuleNameFromPath(p string) string {
	base := p[strings.LastIndex(p, "/")+1:]
	var sb strings.Builder
	for _, r := range base {
		if r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

var validName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateModuleName checks name can be used as a kernel module name.
func ValidateModuleName(name string) error {
	if !validName.MatchString(name) || len(name) > 55 {
		return fmt.Errorf("invalid module name %q: use letters, digits and _ (max 55 chars)", name)
	}
	if name == "shim" || name == "go" {
		return fmt.Errorf("module name %q is reserved, pick another with -name", name)
	}
	return nil
}

// KernelBuildDir resolves the kernel build directory to compile against.
func KernelBuildDir(kver, kdir string) (string, error) {
	if kdir != "" {
		if _, err := os.Stat(filepath.Join(kdir, "Makefile")); err != nil {
			return "", fmt.Errorf("kernel build dir %s: no Makefile", kdir)
		}
		return kdir, nil
	}
	if kver == "" {
		out, err := exec.Command("uname", "-r").Output()
		if err != nil {
			return "", fmt.Errorf("uname -r: %w", err)
		}
		kver = strings.TrimSpace(string(out))
	}
	dir := filepath.Join("/lib/modules", kver, "build")
	if _, err := os.Stat(filepath.Join(dir, "Makefile")); err != nil {
		msg := fmt.Sprintf("no kernel headers for %s (%s missing)", kver, dir)
		if avail := installedKernels(); len(avail) > 0 {
			msg += "\navailable: " + strings.Join(avail, ", ") + " (use -kver)"
		} else {
			msg += "\ninstall your distribution's kernel headers package (e.g. linux-headers)"
		}
		return "", errors.New(msg)
	}
	return dir, nil
}

func installedKernels() []string {
	matches, _ := filepath.Glob("/lib/modules/*/build/Makefile")
	var out []string
	for _, m := range matches {
		out = append(out, filepath.Base(filepath.Dir(filepath.Dir(m))))
	}
	sort.Strings(out)
	return out
}

// CacheDir is where gokm keeps extracted assets and build directories.
func CacheDir() (string, error) {
	if d := os.Getenv("GOKMCACHE"); d != "" {
		return d, nil
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "gokm"), nil
}

// PrepareAssets materializes the assets for the installed TinyGo.
func PrepareAssets() (*assets.Root, error) {
	cache, err := CacheDir()
	if err != nil {
		return nil, err
	}
	tgRoot, tgVersion, err := tinygoInfo()
	if err != nil {
		return nil, err
	}
	return assets.Prepare(cache, tgRoot, tgVersion)
}

func tinygoInfo() (root, version string, err error) {
	if _, err := exec.LookPath("tinygo"); err != nil {
		return "", "", errors.New("tinygo not found in PATH (https://tinygo.org/getting-started/install/)")
	}
	out, err := exec.Command("tinygo", "env", "TINYGOROOT").Output()
	if err != nil {
		return "", "", fmt.Errorf("tinygo env TINYGOROOT: %w", err)
	}
	root = strings.TrimSpace(string(out))
	if os.Getenv("TINYGOROOT") != "" {
		// Don't stack our overlay on top of a user-provided overlay by accident.
		root = os.Getenv("TINYGOROOT")
	}
	out, err = exec.Command("tinygo", "version").Output()
	if err != nil {
		return "", "", fmt.Errorf("tinygo version: %w", err)
	}
	return root, strings.TrimSpace(string(out)), nil
}

// gokmWorkMarker identifies go.work files written by gokm.
const gokmWorkMarker = "// Generated by gokm"

// GoWorkContent returns a go.work using the project and the gokm lib module.
func GoWorkContent(goVersion, proj, lib string, forEditor bool, extra ...string) string {
	var sb strings.Builder
	sb.WriteString(gokmWorkMarker)
	if forEditor {
		sb.WriteString(" so editors can resolve \"gokm/kernel\". `gokm build`\n// rewrites it when needed; it's machine-specific, don't commit it.")
	}
	fmt.Fprintf(&sb, "\n\ngo %s\n\nuse (\n\t%s\n\t%s\n", goVersion, proj, lib)
	for _, e := range extra {
		fmt.Fprintf(&sb, "\t%s\n", e)
	}
	sb.WriteString(")\n")
	return sb.String()
}

// RefreshProjectGoWork updates the project's editor go.work if gokm created it
// and it points to an outdated asset directory.
func RefreshProjectGoWork(proj, goVersion, lib string, extra ...string) {
	p := filepath.Join(proj, "go.work")
	data, err := os.ReadFile(p)
	if err != nil || !bytes.HasPrefix(data, []byte(gokmWorkMarker)) {
		return
	}
	want := GoWorkContent(goVersion, ".", lib, true, extra...)
	if string(data) != want {
		os.WriteFile(p, []byte(want), 0o644)
	}
}

// shortTinyGoVersion turns "tinygo version 0.43.0 linux/amd64 (...)" into "0.43.0".
func shortTinyGoVersion(v string) string {
	if f := strings.Fields(v); len(f) >= 3 && f[0] == "tinygo" {
		return f[2]
	}
	return v
}

func cString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:16]
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
