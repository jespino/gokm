// Package bindgen generates the gokm/linux Go package for a target kernel:
// declarations for the kernel functions, types and constants a project
// references (as linux.X), with exact layouts taken from the kernel's BTF.
//
// Naming: C names become CamelCase Go names with leading underscores dropped
// (si_meminfo -> SiMeminfo, struct super_block -> SuperBlock, field i_mode ->
// IMode). Constants (enum values and macros) keep their C name (HZ,
// GFP_KERNEL, I_NEW).
//
// Mapping:
//   - exported kernel functions (in Module.symvers and BTF): body-less Go
//     functions with //export, i.e. direct calls with the C ABI;
//   - structs/unions/typedef'd structs: Go structs with every field at its
//     BTF offset (explicit padding, size checked at compile time). Fields
//     Go can't express (bitfields, nested structs not referenced themselves,
//     unions) become byte arrays of the right size;
//   - pointers to referenced types are typed (*Inode), other pointers are
//     unsafe.Pointer, function pointers uintptr;
//   - enum constants from BTF; any other name is looked up as a macro and
//     evaluated by compiling it against the kernel headers (see Probe).
package bindgen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"sort"
	"strings"
	"unicode"

	"github.com/jespino/gokm/internal/btf"
)

// Gen resolves names against a kernel's BTF and exports.
type Gen struct {
	spec    *btf.Spec
	exports map[string]string // exported symbol -> namespace

	funcs map[string]*btf.Type // Go name -> FUNC (exported only)
	types map[string]*btf.Type // Go name -> STRUCT/UNION/TYPEDEF
	enums map[string]int64     // C name -> value

	// Set while generating.
	typeName   map[uint32]string    // type ID -> Go name, for generated types
	genTypes   map[string]*btf.Type // Go name -> type to generate
	usedNS     map[string]bool
	needUnsafe bool
	needBF     bool // bitfield helpers
}

// New indexes a kernel's BTF and exported symbols.
func New(spec *btf.Spec, exports map[string]string) *Gen {
	g := &Gen{
		spec: spec, exports: exports,
		funcs: map[string]*btf.Type{}, types: map[string]*btf.Type{}, enums: map[string]int64{},
	}
	better := func(old *btf.Type, new *btf.Type) bool {
		return old == nil || leadingUnderscores(new.Name) < leadingUnderscores(old.Name)
	}
	for _, t := range spec.Types {
		switch t.Kind {
		case btf.KindFunc:
			if _, ok := exports[t.Name]; ok {
				n := GoName(t.Name)
				if better(g.funcs[n], t) {
					g.funcs[n] = t
				}
			}
		case btf.KindStruct, btf.KindUnion:
			if t.Name != "" && len(t.Members) > 0 {
				n := GoName(t.Name)
				if old := g.types[n]; old == nil || old.Kind == btf.KindTypedef || better(old, t) {
					g.types[n] = t
				}
			}
		case btf.KindTypedef:
			if t.Name != "" {
				n := GoName(t.Name)
				if g.types[n] == nil {
					g.types[n] = t
				}
			}
		case btf.KindEnum, btf.KindEnum64:
			for _, e := range t.Enums {
				g.enums[e.Name] = e.Value
			}
		}
	}
	return g
}

func leadingUnderscores(s string) int { return len(s) - len(strings.TrimLeft(s, "_")) }

// GoName converts a C identifier to an exported CamelCase Go name.
func GoName(c string) string {
	var b strings.Builder
	for _, part := range strings.Split(c, "_") {
		if part == "" {
			continue
		}
		r := []rune(part)
		r[0] = unicode.ToUpper(r[0])
		b.WriteString(string(r))
	}
	s := b.String()
	if s == "" || !unicode.IsLetter([]rune(s)[0]) {
		s = "X" + s
	}
	return s
}

// Macros returns the referenced names that are neither exported functions,
// types nor enum constants: static inline functions (probed through DWARF)
// or macro constants (evaluated against the headers).
func (g *Gen) Macros(names []string) []string {
	var out []string
	for _, n := range names {
		if g.funcs[n] == nil && g.types[n] == nil {
			if _, ok := g.enums[n]; !ok {
				out = append(out, n)
			}
		}
	}
	return out
}

// Const is a macro constant's value.
type Const struct {
	Value  int64
	Signed bool
}

// Output is the generated code.
type Output struct {
	Go          []byte   // package linux
	C           []byte   // wrappers compiled with the kernel headers (inline functions, pragmas)
	CStandalone []byte   // wrappers needing no headers (by-value structs of exported functions)
	Namespaces  []string // symbol namespaces to import (MODULE_IMPORT_NS)
}

// Pragma is a //gokm:inline or //gokm:blocking declaration in user code.
type Pragma struct {
	Kind   string // "inline" or "blocking"
	CName  string // the C function or macro to call
	Decl   *ast.FuncDecl
	Linux  string // local name of the gokm/linux import in that file ("" if none)
	Pos    token.Position
	Symbol string // the C wrapper symbol the Go declaration now exports
}

// DefaultHeaders are included by the generated C wrappers and macro probes;
// //gokm:include and the header arguments of pragmas add more.
var DefaultHeaders = []string{
	"linux/module.h", "linux/kernel.h", "linux/types.h", "linux/errno.h", "linux/stat.h",
	"linux/gfp.h", "linux/slab.h", "linux/mm.h", "linux/sched.h", "linux/jiffies.h",
	"linux/delay.h", "linux/fs.h", "linux/uaccess.h", "linux/utsname.h", "linux/cpumask.h",
}

// Includes renders #include lines for the default and extra headers.
func Includes(extra []string) string {
	seen := map[string]bool{}
	var b strings.Builder
	for _, h := range append(append([]string{}, DefaultHeaders...), extra...) {
		if !seen[h] {
			seen[h] = true
			fmt.Fprintf(&b, "#include <%s>\n", h)
		}
	}
	return b.String()
}

// Generate produces the gokm/linux package for the referenced names (with
// macro values and inline-function signatures from the probes), and C
// wrappers for inline functions, by-value structs and pragmas.
func (g *Gen) Generate(names []string, macros map[string]Const, inlines map[string]*InlineDecl,
	pragmas []Pragma, headers []string) (*Output, error) {
	g.typeName = map[uint32]string{}
	g.genTypes = map[string]*btf.Type{}
	g.usedNS = map[string]bool{}
	g.needUnsafe, g.needBF = false, false
	var errs []string

	// Referenced types first: functions and fields refer to them by name.
	for _, n := range names {
		if t := g.types[n]; t != nil && g.funcs[n] == nil {
			g.typeName[t.ID] = n
			g.genTypes[n] = t
			if t.Kind == btf.KindTypedef {
				// Pointers to the underlying struct also get the name.
				if u := g.spec.Skip(t.Ref); u.Kind == btf.KindStruct || u.Kind == btf.KindUnion {
					if _, ok := g.typeName[u.ID]; !ok {
						g.typeName[u.ID] = n
					}
				}
			}
		}
	}

	// Functions (they may add the structs in their signatures).
	var funcs, cHdr, cStd bytes.Buffer
	var fnNames, constNames []string
	for _, n := range names {
		switch {
		case g.funcs[n] != nil, inlines[n] != nil:
			fnNames = append(fnNames, n)
		case g.types[n] != nil:
		default:
			if _, ok := g.enums[n]; ok || macros[n] != (Const{}) || IsConstName(n) {
				constNames = append(constNames, n)
			} else {
				errs = append(errs, fmt.Sprintf("linux.%s: no exported kernel function, type, constant or "+
					"static inline function %s (for function-like macros, declare them with //gokm:inline)", n, CName(n)))
			}
		}
	}
	sort.Strings(fnNames)
	for _, n := range fnNames {
		var sig *funcSig
		var err error
		if fn := g.funcs[n]; fn != nil {
			sig, err = g.btfSig(n, fn)
			if err == nil {
				if ns := g.exports[fn.Name]; ns != "" {
					g.usedNS[ns] = true
				}
			}
		} else {
			sig, err = g.inlineSig(n, inlines[n])
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("linux.%s: %v", n, err))
			continue
		}
		goSrc, ch, cs := g.genFuncCode(sig)
		funcs.WriteString(goSrc)
		cHdr.WriteString(ch)
		cStd.WriteString(cs)
	}

	var body bytes.Buffer
	typeNames := make([]string, 0, len(g.genTypes))
	for n := range g.genTypes {
		typeNames = append(typeNames, n)
	}
	sort.Strings(typeNames)
	for _, n := range typeNames {
		body.WriteString(g.genType(n, g.genTypes[n]))
	}
	body.Write(funcs.Bytes())

	sort.Strings(constNames)
	if len(constNames) > 0 {
		body.WriteString("const (\n")
		for _, n := range constNames {
			if v, ok := g.enums[n]; ok {
				fmt.Fprintf(&body, "\t%s = %d // enum constant\n", n, v)
			} else if c, ok := macros[n]; ok {
				if c.Signed || c.Value >= 0 {
					fmt.Fprintf(&body, "\t%s = %d // macro\n", n, c.Value)
				} else {
					fmt.Fprintf(&body, "\t%s = %d // macro (unsigned)\n", n, uint64(c.Value))
				}
			} else {
				errs = append(errs, fmt.Sprintf("linux.%s: no enum constant or integer macro named %s", n, n))
			}
		}
		body.WriteString(")\n\n")
	}

	for _, p := range pragmas {
		src, err := g.genWrapper(p)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", p.Pos, err))
			continue
		}
		cHdr.WriteString(src)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(errs, "\n"))
	}

	var out bytes.Buffer
	out.WriteString("// Code generated by gokm from the kernel's BTF. DO NOT EDIT.\n\n")
	out.WriteString("// Package linux has the kernel functions, types and constants this module\n// references, for the kernel it was built against.\npackage linux\n\n")
	if g.needUnsafe || len(typeNames) > 0 {
		out.WriteString("import \"unsafe\"\n\nvar _ unsafe.Pointer\n\n")
	}
	if g.needBF {
		out.WriteString(bitfieldHelpers)
	}
	out.Write(body.Bytes())

	formatted, err := format.Source(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("formatting generated bindings: %w\n%s", err, out.Bytes())
	}
	o := &Output{Go: formatted}
	if cHdr.Len() > 0 {
		o.C = append([]byte("// Generated by gokm: wrappers for static inline functions and //gokm: pragmas.\n"+
			Includes(headers)+"#include \"gokm.h\"\n\n"), cHdr.Bytes()...)
	}
	if cStd.Len() > 0 {
		o.CStandalone = append([]byte("// Generated by gokm: wrappers passing structs by value (no kernel headers).\n\n"),
			cStd.Bytes()...)
	}
	for ns := range g.usedNS {
		o.Namespaces = append(o.Namespaces, ns)
	}
	sort.Strings(o.Namespaces)
	return o, nil
}

const bitfieldHelpers = `func b2u(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

func bfLoad(p unsafe.Pointer, n uintptr) uint64 {
	var v uint64
	for i := uintptr(0); i < n; i++ {
		v |= uint64(*(*byte)(unsafe.Add(p, i))) << (8 * i)
	}
	return v
}

func bfStore(p unsafe.Pointer, n uintptr, v uint64) {
	for i := uintptr(0); i < n; i++ {
		*(*byte)(unsafe.Add(p, i)) = byte(v >> (8 * i))
	}
}

`

// goType maps a BTF type to a Go type expression. ok is false for types Go
// code can't use by value (unreferenced structs, variadic, ...).
func (g *Gen) goType(id uint32) (string, bool) {
	t := g.spec.SkipQualifiers(id)
	if n, ok := g.typeName[t.ID]; ok {
		return n, true
	}
	switch t.Kind {
	case btf.KindVoid:
		return "", true
	case btf.KindTypedef:
		return g.goType(t.Ref)
	case btf.KindInt:
		if t.IntEncoding&btf.IntBool != 0 {
			return "bool", true
		}
		signed := t.IntEncoding&btf.IntSigned != 0
		if t.Size == 1 && t.IntEncoding&btf.IntChar != 0 {
			return "byte", true // C char: byte, so char* is *byte and char[] is []byte
		}
		switch t.Size {
		case 1:
			if signed {
				return "int8", true
			}
			return "byte", true
		case 2, 4, 8:
			bits := fmt.Sprint(t.Size * 8)
			if signed {
				return "int" + bits, true
			}
			return "uint" + bits, true
		}
		return "", false // __int128
	case btf.KindEnum, btf.KindEnum64:
		bits := fmt.Sprint(t.Size * 8)
		if t.KindFlag {
			return "int" + bits, true
		}
		return "uint" + bits, true
	case btf.KindFloat:
		switch t.Size {
		case 4:
			return "float32", true
		case 8:
			return "float64", true
		}
		return "", false
	case btf.KindPtr:
		target := g.spec.SkipQualifiers(t.Ref)
		if n, ok := g.typeName[target.ID]; ok {
			return "*" + n, true
		}
		u := g.spec.Skip(t.Ref)
		if n, ok := g.typeName[u.ID]; ok {
			return "*" + n, true
		}
		switch u.Kind {
		case btf.KindFuncProto:
			return "uintptr", true
		case btf.KindInt, btf.KindEnum, btf.KindEnum64, btf.KindFloat:
			if s, ok := g.goType(u.ID); ok {
				return "*" + s, true
			}
		}
		g.needUnsafe = true
		return "unsafe.Pointer", true
	case btf.KindArray:
		if s, ok := g.goType(t.ArrayElem); ok && s != "" {
			return fmt.Sprintf("[%d]%s", t.ArrayLen, s), true
		}
		return fmt.Sprintf("[%d]byte", g.spec.SizeOf(t.ID)), true
	}
	return "", false
}

// alignOf is a type's C alignment.
func (g *Gen) alignOf(id uint32) uint32 {
	t := g.spec.Skip(id)
	switch t.Kind {
	case btf.KindInt, btf.KindEnum, btf.KindEnum64, btf.KindFloat:
		return min(max(t.Size, 1), 8)
	case btf.KindPtr:
		return 8
	case btf.KindArray:
		return g.alignOf(t.ArrayElem)
	case btf.KindStruct, btf.KindUnion:
		a := uint32(1)
		for _, m := range t.Members {
			a = max(a, g.alignOf(m.Type))
		}
		return a
	}
	return 1
}

type field struct {
	name    string
	off     uint32 // bytes
	typ     uint32
	cpath   string
	bitOff  uint32 // bitfields: absolute bit offset and size
	bitSize uint32
}

// flatten lists a struct's or union's members, descending into anonymous
// members (C lets code access their fields directly).
func (g *Gen) flatten(t *btf.Type, base uint32, prefix string, fields, bitfields *[]field) {
	for _, m := range t.Members {
		if m.BitfieldSize != 0 || m.BitOffset%8 != 0 {
			if m.Name != "" {
				size := m.BitfieldSize
				if size == 0 {
					size = 8 * g.spec.SizeOf(m.Type)
				}
				*bitfields = append(*bitfields, field{name: m.Name, typ: m.Type, cpath: prefix + m.Name,
					bitOff: base*8 + m.BitOffset, bitSize: size})
			}
			continue
		}
		off := base + m.BitOffset/8
		if m.Name == "" {
			if u := g.spec.Skip(m.Type); u.Kind == btf.KindStruct || u.Kind == btf.KindUnion {
				g.flatten(u, off, prefix, fields, bitfields)
			}
			continue
		}
		*fields = append(*fields, field{name: m.Name, off: off, typ: m.Type, cpath: prefix + m.Name})
	}
}

func (g *Gen) genType(name string, t *btf.Type) string {
	var b strings.Builder
	cname := t.Name
	under := t
	if t.Kind == btf.KindTypedef {
		under = g.spec.Skip(t.Ref)
		if under.Kind != btf.KindStruct && under.Kind != btf.KindUnion {
			// A scalar typedef: an alias of its Go type.
			if s, ok := g.goType(t.Ref); ok && s != "" {
				return fmt.Sprintf("// %s is C's %s.\ntype %s = %s\n\n", name, cname, name, s)
			}
			return ""
		}
	} else {
		kind := "struct"
		if t.Kind == btf.KindUnion {
			kind = "union"
		}
		cname = kind + " " + t.Name
	}
	size := under.Size
	fmt.Fprintf(&b, "// %s is C's %s (%d bytes).\ntype %s struct {\n", name, cname, size, name)
	switch align := g.alignOf(under.ID); {
	case align >= 8:
		b.WriteString("\t_ [0]uint64 // C alignment\n")
	case align >= 4:
		b.WriteString("\t_ [0]uint32 // C alignment\n")
	}

	var fields, bitfields, overlapped []field
	g.flatten(under, 0, "", &fields, &bitfields)
	sort.SliceStable(fields, func(i, j int) bool { return fields[i].off < fields[j].off })
	if under.Kind == btf.KindUnion {
		overlapped, fields = fields, nil // all members through accessors
	}

	used := map[string]int{}
	cur := uint32(0)
	pad := 0
	for _, f := range fields {
		if f.off < cur {
			overlapped = append(overlapped, f) // union member after the first
			continue
		}
		fsize := g.spec.SizeOf(f.typ)
		if fsize == 0 {
			continue // flexible array
		}
		gt, ok := g.goType(f.typ)
		if !ok || gt == "" || f.off%g.alignOf(f.typ) != 0 {
			gt = fmt.Sprintf("[%d]byte", fsize)
		}
		if f.off > cur {
			fmt.Fprintf(&b, "\t_%d [%d]byte\n", pad, f.off-cur)
			pad++
		}
		fname := GoName(f.name)
		if n := used[fname]; n > 0 {
			fname = fmt.Sprintf("%s_%d", fname, n)
		}
		used[fname]++
		fmt.Fprintf(&b, "\t%s %s // %s, offset %d\n", fname, gt, f.cpath, f.off)
		cur = f.off + fsize
	}
	if cur < size {
		fmt.Fprintf(&b, "\t_%d [%d]byte\n", pad, size-cur)
	}
	b.WriteString("}\n\n")
	// Compile-time layout check.
	fmt.Fprintf(&b, "var _ [%d - unsafe.Sizeof(%s{})]byte\nvar _ [unsafe.Sizeof(%s{}) - %d]byte\n\n", size, name, name, size)

	// Accessors for members that can't be fields: overlapping ones (unions)
	// return a pointer to the member, bitfields get a getter and a setter.
	method := func(n string) bool {
		if used[n] > 0 {
			return false
		}
		used[n]++
		return true
	}
	for _, f := range overlapped {
		fsize := g.spec.SizeOf(f.typ)
		gt, ok := g.goType(f.typ)
		if !ok || gt == "" {
			gt = fmt.Sprintf("[%d]byte", fsize)
		}
		mn := GoName(f.name)
		if fsize == 0 || !method(mn) {
			continue
		}
		fmt.Fprintf(&b, "// %s returns a pointer to %s (offset %d, overlapping other members).\n"+
			"func (s *%s) %s() *%s { return (*%s)(unsafe.Add(unsafe.Pointer(s), %d)) }\n\n",
			mn, f.cpath, f.off, name, mn, gt, gt, f.off)
	}
	for _, f := range bitfields {
		gt, ok := g.goType(f.typ)
		u := g.spec.Skip(f.typ)
		if !ok || (u.Kind != btf.KindInt && u.Kind != btf.KindEnum && u.Kind != btf.KindEnum64) {
			continue
		}
		start, shift := f.bitOff/8, f.bitOff%8
		nbytes := (shift + f.bitSize + 7) / 8
		mn := GoName(f.name)
		if f.bitSize == 0 || nbytes > 8 || !method(mn) || !method("Set"+mn) {
			continue
		}
		g.needBF = true
		mask := fmt.Sprintf("(1<<%d - 1)", f.bitSize)
		if f.bitSize == 64 {
			mask = "^uint64(0)"
		}
		load := fmt.Sprintf("bfLoad(unsafe.Add(unsafe.Pointer(s), %d), %d)", start, nbytes)
		signed := (u.Kind == btf.KindInt && u.IntEncoding&btf.IntSigned != 0) || ((u.Kind == btf.KindEnum || u.Kind == btf.KindEnum64) && u.KindFlag)
		var get string
		switch {
		case gt == "bool":
			get = fmt.Sprintf("return (%s>>%d)&%s != 0", load, shift, mask)
		case signed:
			get = fmt.Sprintf("return %s(int64(%s<<(64-%d)) >> (64 - %d))", gt, load, shift+f.bitSize, f.bitSize)
		default:
			get = fmt.Sprintf("return %s((%s >> %d) & %s)", gt, load, shift, mask)
		}
		val := "uint64(v)"
		if gt == "bool" {
			val = "b2u(v)"
		}
		fmt.Fprintf(&b, "// %s reads the %d-bit bitfield %s.\nfunc (s *%s) %s() %s { %s }\n\n",
			mn, f.bitSize, f.cpath, name, mn, gt, get)
		fmt.Fprintf(&b, "// Set%s writes the bitfield %s.\nfunc (s *%s) Set%s(v %s) {\n"+
			"\tp := unsafe.Add(unsafe.Pointer(s), %d)\n"+
			"\tbfStore(p, %d, bfLoad(p, %d)&^(%s<<%d) | (%s&%s)<<%d)\n}\n\n",
			mn, f.cpath, name, mn, gt, start, nbytes, nbytes, mask, shift, val, mask, shift)
	}
	return b.String()
}

var goKeywords = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true, "default": true,
	"defer": true, "else": true, "fallthrough": true, "for": true, "func": true, "go": true,
	"goto": true, "if": true, "import": true, "interface": true, "map": true, "package": true,
	"range": true, "return": true, "select": true, "struct": true, "switch": true, "type": true,
	"var": true, "len": true, "cap": true, "new": true, "make": true, "string": true, "error": true,
}

// genWrapper writes the C function a //gokm:inline or //gokm:blocking Go
// declaration calls: it forwards to the named C function or macro (with the
// Go lock released around it, for blocking).
func (g *Gen) genWrapper(p Pragma) (string, error) {
	ft := p.Decl.Type
	var params, args []string
	if ft.Params != nil {
		i := 0
		for _, f := range ft.Params.List {
			ct, err := g.cType(f.Type, p.Linux)
			if err != nil {
				return "", err
			}
			n := len(f.Names)
			if n == 0 {
				n = 1
			}
			for j := 0; j < n; j++ {
				params = append(params, fmt.Sprintf("%s a%d", ct, i))
				args = append(args, fmt.Sprintf("a%d", i))
				i++
			}
		}
	}
	ret := "void"
	if ft.Results != nil && len(ft.Results.List) > 0 {
		if len(ft.Results.List) > 1 || len(ft.Results.List[0].Names) > 1 {
			return "", fmt.Errorf("%s: only one result is supported", p.Decl.Name.Name)
		}
		ct, err := g.cType(ft.Results.List[0].Type, p.Linux)
		if err != nil {
			return "", err
		}
		ret = ct
	}
	if len(params) == 0 {
		params = []string{"void"}
	}
	call := fmt.Sprintf("%s(%s)", p.CName, strings.Join(args, ", "))
	var body string
	switch {
	case p.Kind == "blocking" && ret == "void":
		body = fmt.Sprintf("\tGOKM_PARK();\n\t%s;\n\tGOKM_UNPARK();\n", call)
	case p.Kind == "blocking":
		body = fmt.Sprintf("\t%s r;\n\n\tGOKM_PARK();\n\tr = (%s)%s;\n\tGOKM_UNPARK();\n\treturn r;\n", ret, ret, call)
	case ret == "void":
		body = fmt.Sprintf("\t%s;\n", call)
	default:
		body = fmt.Sprintf("\treturn (%s)%s;\n", ret, call)
	}
	return fmt.Sprintf("/* %s: %s */\n%s %s(%s);\n%s %s(%s)\n{\n%s}\n\n",
		p.Pos, p.Decl.Name.Name, ret, p.Symbol, strings.Join(params, ", "),
		ret, p.Symbol, strings.Join(params, ", "), body), nil
}

// cType maps a Go parameter type in a pragma declaration to C.
func (g *Gen) cType(e ast.Expr, linuxName string) (string, error) {
	switch t := e.(type) {
	case *ast.Ident:
		switch t.Name {
		case "int", "int64":
			return "long", nil
		case "uint", "uint64", "uintptr":
			return "unsigned long", nil
		case "int32", "rune":
			return "int", nil
		case "uint32":
			return "unsigned int", nil
		case "int16":
			return "short", nil
		case "uint16":
			return "unsigned short", nil
		case "int8":
			return "signed char", nil
		case "uint8", "byte":
			return "unsigned char", nil
		case "bool":
			return "bool", nil
		case "float32", "float64":
			return "", fmt.Errorf("float parameters aren't supported in //gokm: wrappers (kernel C can't use floating point)")
		}
	case *ast.StarExpr:
		if sel, ok := t.X.(*ast.SelectorExpr); ok {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == linuxName {
				if ty := g.types[sel.Sel.Name]; ty != nil {
					switch ty.Kind {
					case btf.KindStruct:
						return "struct " + ty.Name + " *", nil
					case btf.KindUnion:
						return "union " + ty.Name + " *", nil
					case btf.KindTypedef:
						return ty.Name + " *", nil
					}
				}
			}
		}
		return "void *", nil
	case *ast.SelectorExpr:
		if x, ok := t.X.(*ast.Ident); ok && x.Name == "unsafe" && t.Sel.Name == "Pointer" {
			return "void *", nil
		}
		if x, ok := t.X.(*ast.Ident); ok && x.Name == linuxName {
			if ty := g.types[t.Sel.Name]; ty != nil && ty.Kind == btf.KindTypedef {
				if u := g.spec.Skip(ty.Ref); u.Kind != btf.KindStruct && u.Kind != btf.KindUnion {
					return ty.Name, nil
				}
			}
		}
	}
	return "", fmt.Errorf("unsupported type in //gokm: declaration (use integers, bool, pointers or unsafe.Pointer)")
}
