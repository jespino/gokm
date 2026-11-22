package bindgen

import (
	"debug/dwarf"
	"debug/elf"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/jespino/gokm/internal/btf"
)

// Function signatures, from BTF (exported functions) or DWARF (static inline
// functions, compiled in a probe object), mapped to Go and C.

type sigKind int

const (
	sigVoid   sigKind = iota
	sigScalar         // integers, bool, enums
	sigPtr            // pointers (typed if structID != 0)
	sigStruct         // a struct/union passed or returned by value
)

type sigType struct {
	kind     sigKind
	goType   string // for scalars and untyped pointers
	cType    string // C spelling, for wrappers compiled with the kernel headers
	structID uint32 // BTF type of the pointee / by-value struct
}

type funcSig struct {
	goName, cName string
	inline        bool // static inline: wrapper compiled with the headers
	ret           sigType
	params        []sigType
	pnames        []string
}

// InlineDecl is a static inline function's signature, read from the DWARF
// of an object that takes its address (see ParseInline).
type InlineDecl struct {
	CName      string
	Ret        dwarf.Type // nil: void
	Params     []dwarf.Type
	ParamNames []string
}

// ParseInline reads the signature of cname from an object file's DWARF.
func ParseInline(path, cname string) (*InlineDecl, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	d, err := f.DWARF()
	if err != nil {
		return nil, err
	}
	r := d.Reader()
	for {
		e, err := r.Next()
		if err != nil {
			return nil, err
		}
		if e == nil {
			return nil, fmt.Errorf("%s not found in the DWARF of %s", cname, path)
		}
		if e.Tag != dwarf.TagSubprogram {
			continue
		}
		if name, _ := e.Val(dwarf.AttrName).(string); name != cname {
			if e.Children {
				r.SkipChildren()
			}
			continue
		}
		decl := &InlineDecl{CName: cname}
		if off, ok := e.Val(dwarf.AttrType).(dwarf.Offset); ok {
			if decl.Ret, err = d.Type(off); err != nil {
				return nil, err
			}
		}
		if e.Children {
			for {
				c, err := r.Next()
				if err != nil {
					return nil, err
				}
				if c == nil || c.Tag == 0 {
					break
				}
				switch c.Tag {
				case dwarf.TagFormalParameter:
					off, _ := c.Val(dwarf.AttrType).(dwarf.Offset)
					t, err := d.Type(off)
					if err != nil {
						return nil, err
					}
					name, _ := c.Val(dwarf.AttrName).(string)
					decl.Params = append(decl.Params, t)
					decl.ParamNames = append(decl.ParamNames, name)
				case dwarf.TagUnspecifiedParameters:
					return nil, fmt.Errorf("%s is variadic", cname)
				}
				if c.Children {
					r.SkipChildren()
				}
			}
		}
		return decl, nil
	}
}

// CName guesses the C name of a Go name for an inline function:
// NumOnlineCpus -> num_online_cpus, DInode -> d_inode.
func CName(goName string) string {
	r := []rune(goName)
	var b strings.Builder
	for i, c := range r {
		if unicode.IsUpper(c) && i > 0 {
			prevLower := unicode.IsLower(r[i-1]) || unicode.IsDigit(r[i-1])
			nextLower := i+1 < len(r) && unicode.IsLower(r[i+1])
			if prevLower || (unicode.IsUpper(r[i-1]) && nextLower) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToLower(c))
	}
	return b.String()
}

// IsConstName reports whether a name looks like a macro constant (HZ,
// PAGE_SIZE) rather than a CamelCase function name.
func IsConstName(n string) bool {
	for _, c := range n {
		if !unicode.IsUpper(c) && !unicode.IsDigit(c) && c != '_' {
			return false
		}
	}
	return true
}

func scalarGo(size int64, signed, char, boolean bool) (string, error) {
	switch {
	case boolean:
		return "bool", nil
	case size == 1 && (char || !signed):
		return "byte", nil
	case size == 1:
		return "int8", nil
	case size == 2 || size == 4 || size == 8:
		if signed {
			return fmt.Sprintf("int%d", size*8), nil
		}
		return fmt.Sprintf("uint%d", size*8), nil
	}
	return "", fmt.Errorf("unsupported %d-byte integer", size)
}

// cScalar spells a Go scalar type in C (same size and signedness).
func cScalar(goType string) string {
	switch goType {
	case "bool":
		return "_Bool"
	case "byte":
		return "unsigned char"
	case "int8":
		return "signed char"
	case "int16":
		return "short"
	case "uint16":
		return "unsigned short"
	case "int32":
		return "int"
	case "uint32":
		return "unsigned int"
	case "int64":
		return "long long"
	case "uint64":
		return "unsigned long long"
	case "uintptr":
		return "unsigned long"
	}
	return "void *" // pointers
}

func (g *Gen) structID(kind btf.Kind, name string) uint32 {
	if t := g.types[GoName(name)]; t != nil && t.Kind == kind && t.Name == name {
		return t.ID
	}
	for _, t := range g.spec.Types {
		if t.Kind == kind && t.Name == name && len(t.Members) > 0 {
			return t.ID
		}
	}
	return 0
}

// sigFromBTF maps a BTF parameter/return type.
func (g *Gen) sigFromBTF(id uint32) (sigType, error) {
	t := g.spec.SkipQualifiers(id)
	switch t.Kind {
	case btf.KindVoid:
		return sigType{kind: sigVoid, cType: "void"}, nil
	case btf.KindTypedef:
		s, err := g.sigFromBTF(t.Ref)
		s.cType = t.Name
		return s, err
	case btf.KindInt, btf.KindEnum, btf.KindEnum64:
		gt, ok := g.goType(t.ID)
		if !ok {
			return sigType{}, errors.New("unsupported integer type")
		}
		return sigType{kind: sigScalar, goType: gt, cType: cScalar(gt)}, nil
	case btf.KindFloat:
		return sigType{}, errors.New("floating-point parameters aren't supported")
	case btf.KindPtr:
		u := g.spec.Skip(t.Ref)
		switch {
		case (u.Kind == btf.KindStruct || u.Kind == btf.KindUnion) && u.Name != "" && len(u.Members) > 0:
			return sigType{kind: sigPtr, structID: u.ID, cType: "void *"}, nil
		case u.Kind == btf.KindFuncProto:
			return sigType{kind: sigPtr, goType: "uintptr", cType: "void *"}, nil
		case u.Kind == btf.KindInt && u.Size == 1:
			return sigType{kind: sigPtr, goType: "*byte", cType: "void *"}, nil
		case u.Kind == btf.KindInt || u.Kind == btf.KindEnum || u.Kind == btf.KindEnum64:
			gt, _ := g.goType(u.ID)
			return sigType{kind: sigPtr, goType: "*" + gt, cType: "void *"}, nil
		}
		return sigType{kind: sigPtr, goType: "unsafe.Pointer", cType: "void *"}, nil
	case btf.KindStruct, btf.KindUnion:
		if t.Name == "" {
			return sigType{}, errors.New("anonymous struct passed by value")
		}
		return sigType{kind: sigStruct, structID: t.ID}, nil
	}
	return sigType{}, fmt.Errorf("unsupported parameter type (BTF kind %d)", t.Kind)
}

// cSpell spells a DWARF type in C.
func cSpell(t dwarf.Type) (string, error) {
	switch t := t.(type) {
	case nil, *dwarf.VoidType:
		return "void", nil
	case *dwarf.QualType:
		s, err := cSpell(t.Type)
		return t.Qual + " " + s, err
	case *dwarf.TypedefType:
		return t.Name, nil
	case *dwarf.StructType:
		if t.StructName == "" {
			return "", errors.New("anonymous struct")
		}
		return t.Kind + " " + t.StructName, nil
	case *dwarf.EnumType:
		if t.EnumName == "" {
			return "int", nil
		}
		return "enum " + t.EnumName, nil
	case *dwarf.PtrType:
		if _, ok := stripDWARF(t.Type).(*dwarf.FuncType); ok {
			return "void *", nil
		}
		s, err := cSpell(t.Type)
		return s + " *", err
	case *dwarf.FuncType:
		return "", errors.New("function type")
	}
	return t.Common().Name, nil // base types: "unsigned int", "_Bool", ...
}

func stripDWARF(t dwarf.Type) dwarf.Type {
	for {
		switch tt := t.(type) {
		case *dwarf.QualType:
			t = tt.Type
		case *dwarf.TypedefType:
			t = tt.Type
		default:
			return t
		}
	}
}

// sigFromDWARF maps a DWARF parameter/return type.
func (g *Gen) sigFromDWARF(t dwarf.Type) (sigType, error) {
	spell, err := cSpell(t)
	if err != nil {
		return sigType{}, err
	}
	switch u := stripDWARF(t).(type) {
	case nil, *dwarf.VoidType:
		return sigType{kind: sigVoid, cType: "void"}, nil
	case *dwarf.BoolType:
		return sigType{kind: sigScalar, goType: "bool", cType: spell}, nil
	case *dwarf.CharType, *dwarf.UcharType:
		return sigType{kind: sigScalar, goType: "byte", cType: spell}, nil
	case *dwarf.IntType:
		gt, err := scalarGo(u.ByteSize, true, false, false)
		return sigType{kind: sigScalar, goType: gt, cType: spell}, err
	case *dwarf.UintType:
		gt, err := scalarGo(u.ByteSize, false, false, false)
		return sigType{kind: sigScalar, goType: gt, cType: spell}, err
	case *dwarf.EnumType:
		signed := false
		for _, v := range u.Val {
			if v.Val < 0 {
				signed = true
			}
		}
		gt, err := scalarGo(u.ByteSize, signed, false, false)
		return sigType{kind: sigScalar, goType: gt, cType: spell}, err
	case *dwarf.PtrType:
		switch e := stripDWARF(u.Type).(type) {
		case *dwarf.StructType:
			kind := btf.KindStruct
			if e.Kind == "union" {
				kind = btf.KindUnion
			}
			if id := g.structID(kind, e.StructName); id != 0 {
				return sigType{kind: sigPtr, structID: id, cType: spell}, nil
			}
		case *dwarf.FuncType:
			return sigType{kind: sigPtr, goType: "uintptr", cType: spell}, nil
		case *dwarf.CharType, *dwarf.UcharType:
			return sigType{kind: sigPtr, goType: "*byte", cType: spell}, nil
		}
		return sigType{kind: sigPtr, goType: "unsafe.Pointer", cType: spell}, nil
	case *dwarf.StructType:
		kind := btf.KindStruct
		if u.Kind == "union" {
			kind = btf.KindUnion
		}
		if id := g.structID(kind, u.StructName); id != 0 {
			return sigType{kind: sigStruct, structID: id, cType: spell}, nil
		}
		return sigType{}, fmt.Errorf("%s isn't in the kernel's BTF", spell)
	case *dwarf.FloatType:
		return sigType{}, errors.New("floating-point parameters aren't supported")
	}
	return sigType{}, fmt.Errorf("unsupported type %s", spell)
}

// needType makes sure a struct used in a signature gets a Go type.
func (g *Gen) needType(id uint32) string {
	if n, ok := g.typeName[id]; ok {
		return n
	}
	t := g.spec.Type(id)
	n := GoName(t.Name)
	if other := g.genTypes[n]; other != nil && other.ID != id {
		n += "Struct"
	}
	g.typeName[id] = n
	g.genTypes[n] = t
	return n
}

func (g *Gen) goOf(s sigType) string {
	switch s.kind {
	case sigVoid:
		return ""
	case sigPtr:
		if s.structID != 0 {
			return "*" + g.needType(s.structID)
		}
		if s.goType == "unsafe.Pointer" {
			g.needUnsafe = true
		}
		return s.goType
	case sigStruct:
		return g.needType(s.structID)
	}
	return s.goType
}

func (g *Gen) btfSig(goName string, fn *btf.Type) (*funcSig, error) {
	proto := g.spec.Type(fn.Ref)
	if proto == nil || proto.Kind != btf.KindFuncProto {
		return nil, fmt.Errorf("no prototype for %s", fn.Name)
	}
	sig := &funcSig{goName: goName, cName: fn.Name}
	var err error
	if sig.ret, err = g.sigFromBTF(proto.Ref); err != nil {
		return nil, fmt.Errorf("%s: return type: %v", fn.Name, err)
	}
	for i, p := range proto.Params {
		if p.Type == 0 {
			return nil, fmt.Errorf("%s is variadic, which Go can't call; wrap it with //gokm:inline", fn.Name)
		}
		s, err := g.sigFromBTF(p.Type)
		if err != nil {
			return nil, fmt.Errorf("%s: parameter %d: %v", fn.Name, i, err)
		}
		sig.params = append(sig.params, s)
		sig.pnames = append(sig.pnames, p.Name)
	}
	return sig, nil
}

func (g *Gen) inlineSig(goName string, d *InlineDecl) (*funcSig, error) {
	sig := &funcSig{goName: goName, cName: d.CName, inline: true}
	var err error
	if sig.ret, err = g.sigFromDWARF(d.Ret); err != nil {
		return nil, fmt.Errorf("%s: return type: %v", d.CName, err)
	}
	for i, t := range d.Params {
		s, err := g.sigFromDWARF(t)
		if err != nil {
			return nil, fmt.Errorf("%s: parameter %d: %v", d.CName, i, err)
		}
		sig.params = append(sig.params, s)
		sig.pnames = append(sig.pnames, d.ParamNames[i])
	}
	return sig, nil
}

func paramName(n string, i int) string {
	if n == "" || goKeywords[n] || n == "unsafe" || n == "out" || n == "r" {
		return fmt.Sprintf("p%d", i)
	}
	return n
}

// genFuncCode generates the Go declaration(s) for a function and, when
// needed, the C wrapper: standalone (exported functions with by-value
// structs) or compiled with the headers (static inline functions).
func (g *Gen) genFuncCode(sig *funcSig) (goSrc, cHeaders, cStandalone string) {
	byval := sig.ret.kind == sigStruct
	for _, p := range sig.params {
		byval = byval || p.kind == sigStruct
	}
	ret := g.goOf(sig.ret)
	var params, args []string
	for i, p := range sig.params {
		n := paramName(sig.pnames[i], i)
		params = append(params, n+" "+g.goOf(p))
		if p.kind == sigStruct {
			args = append(args, "&"+n)
		} else {
			args = append(args, n)
		}
	}
	doc := fmt.Sprintf("// %s calls the kernel function %s.\n", sig.goName, sig.cName)
	if sig.inline {
		doc = fmt.Sprintf("// %s calls the kernel's static inline function %s (through a C wrapper).\n", sig.goName, sig.cName)
	}

	if !byval && !sig.inline {
		return fmt.Sprintf("%s//\n//export %s\nfunc %s(%s) %s\n\n",
			doc, sig.cName, sig.goName, strings.Join(params, ", "), ret), "", ""
	}

	sym := "gokm_auto_" + sig.cName
	if !sig.inline {
		sym = "gokm_bv_" + sig.cName
	}
	if !byval {
		// Inline function, plain signature: call the wrapper directly.
		return fmt.Sprintf("%s//\n//export %s\nfunc %s(%s) %s\n\n", doc, sym, sig.goName,
			strings.Join(params, ", "), ret), g.cWrapper(sig, sym), ""
	}

	// By-value structs: the wrapper takes pointers; a Go function with a
	// body keeps the by-value API.
	var wparams []string
	if sig.ret.kind == sigStruct {
		wparams = append(wparams, "out *"+ret)
	}
	for i, p := range sig.params {
		n := paramName(sig.pnames[i], i)
		gt := g.goOf(p)
		if p.kind == sigStruct {
			gt = "*" + gt
		}
		wparams = append(wparams, n+" "+gt)
	}
	wname := strings.ToLower(sig.goName[:1]) + sig.goName[1:] + "Wrapper"
	wret := ret
	if sig.ret.kind == sigStruct {
		wret = ""
	}
	src := fmt.Sprintf("//export %s\nfunc %s(%s) %s\n\n", sym, wname, strings.Join(wparams, ", "), wret)
	src += fmt.Sprintf("%sfunc %s(%s) %s {\n", doc, sig.goName, strings.Join(params, ", "), ret)
	switch sig.ret.kind {
	case sigStruct:
		src += fmt.Sprintf("\tvar r %s\n\t%s(%s)\n\treturn r\n}\n\n", ret, wname, strings.Join(append([]string{"&r"}, args...), ", "))
	case sigVoid:
		src += fmt.Sprintf("\t%s(%s)\n}\n\n", wname, strings.Join(args, ", "))
	default:
		src += fmt.Sprintf("\treturn %s(%s)\n}\n\n", wname, strings.Join(args, ", "))
	}
	if sig.inline {
		return src, g.cWrapper(sig, sym), ""
	}
	return src, "", g.cStandaloneWrapper(sig, sym)
}

// cWrapper is a wrapper compiled with the kernel headers (inline functions).
func (g *Gen) cWrapper(sig *funcSig, sym string) string {
	var params, args []string
	if sig.ret.kind == sigStruct {
		params = append(params, sig.ret.cType+" *out")
	}
	for i, p := range sig.params {
		if p.kind == sigStruct {
			params = append(params, fmt.Sprintf("%s *a%d", p.cType, i))
			args = append(args, fmt.Sprintf("*a%d", i))
		} else {
			params = append(params, fmt.Sprintf("%s a%d", p.cType, i))
			args = append(args, fmt.Sprintf("a%d", i))
		}
	}
	if len(params) == 0 {
		params = []string{"void"}
	}
	call := fmt.Sprintf("%s(%s)", sig.cName, strings.Join(args, ", "))
	ret, body := sig.ret.cType, "\treturn "+call+";\n"
	switch sig.ret.kind {
	case sigStruct:
		ret, body = "void", "\t*out = "+call+";\n"
	case sigVoid:
		ret, body = "void", "\t"+call+";\n"
	}
	decl := fmt.Sprintf("%s %s(%s)", ret, sym, strings.Join(params, ", "))
	return fmt.Sprintf("%s;\n%s\n{\n%s}\n\n", decl, decl, body)
}

// cStandaloneWrapper is a wrapper for an exported function with by-value
// structs that doesn't need the kernel headers: structs are mirrored as
// byte blobs of the same size and alignment (for the kernel's integer-only
// structs the x86-64 ABI passes those identically) and the function is
// declared with the mirrors.
func (g *Gen) cStandaloneWrapper(sig *funcSig, sym string) string {
	var b strings.Builder
	mirror := func(id uint32) string {
		t := g.spec.Type(id)
		name := fmt.Sprintf("gokm_mirror_%d", id)
		fmt.Fprintf(&b, "struct %s { unsigned char b[%d]; } __attribute__((aligned(%d)));\n",
			name, t.Size, g.alignOf(id))
		return "struct " + name
	}
	ctype := func(s sigType) string {
		switch s.kind {
		case sigStruct:
			return mirror(s.structID)
		case sigVoid:
			return "void"
		case sigPtr:
			return "void *"
		}
		return cScalar(s.goType)
	}
	ret := ctype(sig.ret)
	var protoParams, params, args []string
	if sig.ret.kind == sigStruct {
		params = append(params, ret+" *out")
	}
	for i, p := range sig.params {
		ct := ctype(p)
		protoParams = append(protoParams, ct)
		if p.kind == sigStruct {
			params = append(params, fmt.Sprintf("%s *a%d", ct, i))
			args = append(args, fmt.Sprintf("*a%d", i))
		} else {
			params = append(params, fmt.Sprintf("%s a%d", ct, i))
			args = append(args, fmt.Sprintf("a%d", i))
		}
	}
	if len(protoParams) == 0 {
		protoParams = []string{"void"}
	}
	if len(params) == 0 {
		params = []string{"void"}
	}
	fmt.Fprintf(&b, "extern %s %s(%s);\n", ret, sig.cName, strings.Join(protoParams, ", "))
	call := fmt.Sprintf("%s(%s)", sig.cName, strings.Join(args, ", "))
	wret, body := ret, "\treturn "+call+";\n"
	switch sig.ret.kind {
	case sigStruct:
		wret, body = "void", "\t*out = "+call+";\n"
	case sigVoid:
		body = "\t" + call + ";\n"
	}
	decl := fmt.Sprintf("%s %s(%s)", wret, sym, strings.Join(params, ", "))
	fmt.Fprintf(&b, "%s;\n%s\n{\n%s}\n\n", decl, decl, body)
	return b.String()
}
