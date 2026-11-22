package build

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/jespino/gokm/internal/bindgen"
	"github.com/jespino/gokm/internal/btf"
)

// LinuxPkg is the import path of the generated kernel bindings.
const LinuxPkg = "gokm/linux"

// sourceScan is what gokm found in the project's Go files.
type sourceScan struct {
	names   []string // referenced linux.X names
	pragmas []bindgen.Pragma
	headers []string // from //gokm:include and pragma arguments
	params  []moduleParam // //gokm:param variables
	root    string        // project directory
}

// prepareSource copies the project's module into dst, rewriting gokm
// pragmas into //export directives (keeping line numbers), and collects the
// gokm/linux names and pragmas it uses.
//
//	//gokm:inline cname [header.h ...]    call a static inline function or macro
//	//gokm:blocking cname [header.h ...]  same, releasing the Go lock around it
//	//gokm:include header.h ...           headers for the above and for macros
//
// The pragmas go in the doc comment of a body-less Go function, whose
// signature is the one the C function is called with.
func prepareSource(proj, dst string) (*sourceScan, error) {
	if err := os.RemoveAll(dst); err != nil {
		return nil, err
	}
	scan := &sourceScan{root: filepath.Clean(proj)}
	names := map[string]bool{}
	err := filepath.WalkDir(proj, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(proj, p)
		if d.IsDir() {
			if rel != "." && (strings.HasPrefix(d.Name(), ".") || strings.HasPrefix(d.Name(), "_") || d.Name() == CSrcDir) {
				return filepath.SkipDir
			}
			if rel != "." {
				if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir // a separate module
				}
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if strings.HasSuffix(p, ".ko") || !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			if data, err = scanFile(p, data, scan, names); err != nil {
				return err
			}
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0o644)
	})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, p := range scan.params {
		if seen[p.Name] {
			return nil, fmt.Errorf("%s: duplicate //gokm:param %s", p.Pos, p.Name)
		}
		seen[p.Name] = true
	}
	if err := writeParamsGo(dst, scan.params); err != nil {
		return nil, err
	}
	for n := range names {
		scan.names = append(scan.names, n)
	}
	sort.Strings(scan.names)
	return scan, nil
}

func scanFile(path string, src []byte, scan *sourceScan, names map[string]bool) ([]byte, error) {
	if !strings.Contains(string(src), LinuxPkg) && !strings.Contains(string(src), "//gokm:") {
		return src, nil
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	linux := ""
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == LinuxPkg {
			linux = "linux"
			if imp.Name != nil {
				linux = imp.Name.Name
			}
		}
	}
	if linux != "" {
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == linux && x.Obj == nil {
					names[sel.Sel.Name] = true
				}
			}
			return true
		})
	}

	if filepath.Dir(path) == scan.root {
		params, err := scanParams(f, fset, path)
		if err != nil {
			return nil, err
		}
		scan.params = append(scan.params, params...)
	}

	type edit struct {
		start, end int
		text       string
	}
	var edits []edit
	// File-level //gokm:include anywhere.
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if args, ok := strings.CutPrefix(c.Text, "//gokm:include "); ok {
				scan.headers = append(scan.headers, strings.Fields(args)...)
			}
		}
	}
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Doc == nil {
			continue
		}
		for _, c := range fd.Doc.List {
			var kind, args string
			switch {
			case strings.HasPrefix(c.Text, "//gokm:inline "):
				kind, args = "inline", strings.TrimPrefix(c.Text, "//gokm:inline ")
			case strings.HasPrefix(c.Text, "//gokm:blocking "):
				kind, args = "blocking", strings.TrimPrefix(c.Text, "//gokm:blocking ")
			default:
				continue
			}
			pos := fset.Position(c.Pos())
			fields := strings.Fields(args)
			if len(fields) == 0 {
				return nil, fmt.Errorf("%s: //gokm:%s needs the C function name", pos, kind)
			}
			if fd.Body != nil {
				return nil, fmt.Errorf("%s: //gokm:%s functions must not have a body", pos, kind)
			}
			if fd.Recv != nil {
				return nil, fmt.Errorf("%s: //gokm:%s can't be used on methods", pos, kind)
			}
			sym := "gokm_" + kind + "_" + fields[0]
			scan.pragmas = append(scan.pragmas, bindgen.Pragma{
				Kind: kind, CName: fields[0], Decl: fd, Linux: linux, Pos: pos, Symbol: sym,
			})
			scan.headers = append(scan.headers, fields[1:]...)
			start := fset.Position(c.Pos()).Offset
			edits = append(edits, edit{start, start + len(c.Text), "//export " + sym})
		}
	}
	if len(edits) == 0 {
		return src, nil
	}
	out := append([]byte{}, src...)
	for i := len(edits) - 1; i >= 0; i-- { // positions ascend: apply from the end
		e := edits[i]
		out = append(out[:e.start], append([]byte(e.text), out[e.end:]...)...)
	}
	return out, nil
}

// btfPath finds the BTF of the kernel being built for.
func btfPath(kdir string) (string, error) {
	if p := filepath.Join(kdir, "vmlinux"); fileExists(p) {
		return p, nil
	}
	// The running kernel's, if that's the target.
	if rel, err := exec.Command("uname", "-r").Output(); err == nil {
		running := filepath.Join("/lib/modules", strings.TrimSpace(string(rel)), "build")
		if r1, _ := filepath.EvalSymlinks(running); r1 != "" {
			if r2, _ := filepath.EvalSymlinks(kdir); r1 == r2 && fileExists("/sys/kernel/btf/vmlinux") {
				return "/sys/kernel/btf/vmlinux", nil
			}
		}
	}
	return "", fmt.Errorf("no BTF for %s: need %s/vmlinux (kernel headers with BTF) to use %s",
		kdir, kdir, LinuxPkg)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// generateBindings writes the gokm/linux module into genDir and returns the
// C wrappers and namespaces to import.
func (b *builder) generateBindings(scan *sourceScan, kdir, work, genDir string, exports map[string]string) (*bindgen.Output, error) {
	path, err := btfPath(kdir)
	if err != nil {
		return nil, err
	}
	spec, err := btf.Load(path)
	if err != nil {
		return nil, fmt.Errorf("loading BTF: %w", err)
	}
	g := bindgen.New(spec, exports)
	// Names that aren't exported functions, types or enum constants: ALL_CAPS
	// ones are macro constants, CamelCase ones static inline functions.
	var macroNames, inlineNames []string
	for _, n := range g.Macros(scan.names) {
		if bindgen.IsConstName(n) {
			macroNames = append(macroNames, n)
		} else {
			inlineNames = append(inlineNames, n)
		}
	}
	macros := map[string]bindgen.Const{}
	if len(macroNames) > 0 {
		if macros, err = b.probeMacros(kdir, filepath.Join(work, "probe"), macroNames, scan.headers); err != nil {
			return nil, err
		}
	}
	inlines := map[string]*bindgen.InlineDecl{}
	if len(inlineNames) > 0 {
		if inlines, err = b.probeInlines(kdir, filepath.Join(work, "probe-inline"), inlineNames, scan.headers); err != nil {
			return nil, err
		}
	}
	out, err := g.Generate(scan.names, macros, inlines, scan.pragmas, scan.headers)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		return nil, err
	}
	mod := fmt.Sprintf("module %s\n\ngo 1.22\n", LinuxPkg)
	if err := os.WriteFile(filepath.Join(genDir, "go.mod"), []byte(mod), 0o644); err != nil {
		return nil, err
	}
	return out, os.WriteFile(filepath.Join(genDir, "linux.go"), out.Go, 0o644)
}

// probeInlines finds the signatures of static inline functions: for each
// one it compiles (with kbuild, so the kernel's flags and -g) a file taking
// its address, which makes the compiler emit it with DWARF, and reads the
// signature from there. Names that don't compile are left out (the
// generator reports them).
func (b *builder) probeInlines(kdir, dir string, goNames, headers []string) (map[string]*bindgen.InlineDecl, error) {
	b.step("looking up static inline functions: %s", strings.Join(goNames, " "))
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	includes := bindgen.Includes(headers)
	var objs []string
	for i, n := range goNames {
		src := fmt.Sprintf("%svoid *gokm_ref_%d = (void *)&%s;\n", includes, i, bindgen.CName(n))
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("inl_%d.c", i)), []byte(src), 0o644); err != nil {
			return nil, err
		}
		objs = append(objs, fmt.Sprintf("inl_%d.o", i))
	}
	kbuild := "obj-m := " + strings.Join(objs, " ") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "Kbuild"), []byte(kbuild), 0o644); err != nil {
		return nil, err
	}
	args := append([]string{"-k", fmt.Sprintf("-j%d", runtime.NumCPU()), "-C", kdir, "M=" + dir}, objs...)
	b.exec(exec.Command("make", args...)) // failures: names that aren't inline functions

	out := map[string]*bindgen.InlineDecl{}
	for i, n := range goNames {
		obj := filepath.Join(dir, objs[i])
		if !fileExists(obj) {
			continue
		}
		d, err := bindgen.ParseInline(obj, bindgen.CName(n))
		if err != nil {
			return nil, fmt.Errorf("linux.%s: %v", n, err)
		}
		out[n] = d
	}
	return out, nil
}

// probeMacros evaluates integer macros by compiling them against the kernel
// headers (with kbuild, so with the kernel's exact flags and config) and
// reading the values from the object file.
func (b *builder) probeMacros(kdir, dir string, names, headers []string) (map[string]bindgen.Const, error) {
	b.step("evaluating kernel macros: %s", strings.Join(names, " "))
	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var src strings.Builder
	src.WriteString(bindgen.Includes(headers))
	for _, n := range names {
		fmt.Fprintf(&src, "#ifdef %[1]s\nconst long long gokm_const_%[1]s = (long long)(%[1]s);\n"+
			"const int gokm_signed_%[1]s = ((__typeof__(%[1]s))-1 < 0);\n#endif\n", n)
	}
	if err := os.WriteFile(filepath.Join(dir, "probe.c"), []byte(src.String()), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "Kbuild"), []byte("obj-m := probe.o\n"), 0o644); err != nil {
		return nil, err
	}
	if err := b.exec(exec.Command("make", "-C", kdir, "M="+dir, "probe.o")); err != nil {
		return nil, fmt.Errorf("evaluating macros %s (they must be integer constants): %w", strings.Join(names, ", "), err)
	}

	f, err := elf.Open(filepath.Join(dir, "probe.o"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	syms, err := f.Symbols()
	if err != nil {
		return nil, err
	}
	read := func(s elf.Symbol, size int) (uint64, bool) {
		if int(s.Section) >= len(f.Sections) {
			return 0, false
		}
		data, err := f.Sections[s.Section].Data()
		if err != nil || int(s.Value)+size > len(data) {
			return 0, false
		}
		if size == 4 {
			return uint64(binary.LittleEndian.Uint32(data[s.Value:])), true
		}
		return binary.LittleEndian.Uint64(data[s.Value:]), true
	}
	values := map[string]bindgen.Const{}
	signed := map[string]bool{}
	for _, s := range syms {
		if n, ok := strings.CutPrefix(s.Name, "gokm_const_"); ok {
			if v, ok := read(s, 8); ok {
				c := values[n]
				c.Value = int64(v)
				values[n] = c
			}
		} else if n, ok := strings.CutPrefix(s.Name, "gokm_signed_"); ok {
			if v, ok := read(s, 4); ok {
				signed[n] = v != 0
			}
		}
	}
	for n, c := range values {
		c.Signed = signed[n]
		values[n] = c
	}
	return values, nil
}
