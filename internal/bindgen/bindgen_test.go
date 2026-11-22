package bindgen

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jespino/gokm/internal/btf"
)

func TestGoName(t *testing.T) {
	for c, want := range map[string]string{
		"si_meminfo":        "SiMeminfo",
		"super_block":       "SuperBlock",
		"i_mode":            "IMode",
		"__kmalloc":         "Kmalloc",
		"new_utsname":       "NewUtsname",
		"ktime_get_seconds": "KtimeGetSeconds",
		"_1":                "X1",
	} {
		if got := GoName(c); got != want {
			t.Errorf("GoName(%q) = %q, want %q", c, got, want)
		}
	}
}

// TestGenerateRunningKernel generates bindings from the running kernel's
// BTF (when available) and checks the layout of a well-known struct.
func TestGenerateRunningKernel(t *testing.T) {
	spec, err := btf.Load("/sys/kernel/btf/vmlinux")
	if err != nil {
		t.Skip("no kernel BTF:", err)
	}
	exports := map[string]string{"si_meminfo": "", "kstrtouint": ""}
	g := New(spec, exports)
	names := []string{"Sysinfo", "SiMeminfo", "Kstrtouint", "I_NEW", "HZ"}
	if m := g.Macros(names); len(m) != 1 || m[0] != "HZ" {
		t.Fatalf("Macros = %v, want [HZ]", m)
	}
	out, err := g.Generate(names, map[string]Const{"HZ": {Value: 1000, Signed: true}}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	src := string(out.Go)
	for _, want := range []string{
		"type Sysinfo struct {",
		"Totalram  uint64",
		"// totalram, offset 32",
		"MemUnit   uint32 // mem_unit, offset 104",
		"var _ [112 - unsafe.Sizeof(Sysinfo{})]byte",
		"//export si_meminfo\nfunc SiMeminfo(val *Sysinfo)",
		"func Kstrtouint(s *byte, base uint32, res *uint32) int32",
		"HZ    = 1000",
		"I_NEW = 1",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("generated code lacks %q", want)
		}
	}
	if t.Failed() {
		os.WriteFile(t.TempDir()+"/linux.go", out.Go, 0o644)
		t.Log(src)
	}
}

// TestBitfieldsAndUnions generates bindings for a synthetic struct with
// bitfields and an anonymous union, then compiles and runs a Go test that
// checks the accessors bit for bit.
func TestBitfieldsAndUnions(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain")
	}
	spec := &btf.Spec{Types: []*btf.Type{
		{Kind: btf.KindVoid},
		{ID: 1, Kind: btf.KindInt, Name: "unsigned int", Size: 4, IntBits: 32},
		{ID: 2, Kind: btf.KindInt, Name: "int", Size: 4, IntBits: 32, IntEncoding: btf.IntSigned},
		{ID: 3, Kind: btf.KindInt, Name: "unsigned long long", Size: 8, IntBits: 64},
		{ID: 4, Kind: btf.KindStruct, Name: "flags", Size: 16, KindFlag: true, Members: []btf.Member{
			{Name: "a", Type: 1, BitOffset: 0, BitfieldSize: 3},
			{Name: "b", Type: 2, BitOffset: 3, BitfieldSize: 5},
			{Name: "c", Type: 1, BitOffset: 8, BitfieldSize: 13},
			{Name: "d", Type: 3, BitOffset: 64},
		}},
		{ID: 5, Kind: btf.KindUnion, Size: 8, Members: []btf.Member{
			{Name: "x", Type: 3}, {Name: "y", Type: 1},
		}},
		{ID: 6, Kind: btf.KindStruct, Name: "holder", Size: 16, Members: []btf.Member{
			{Name: "n", Type: 1, BitOffset: 0}, {Name: "", Type: 5, BitOffset: 64},
		}},
	}}
	out, err := New(spec, nil).Generate([]string{"Flags", "Holder"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	check := `package linux

import (
	"testing"
	"unsafe"
)

func TestAccessors(t *testing.T) {
	var f Flags
	f.SetA(5)
	f.SetB(-7)
	f.SetC(0x1abc)
	f.D = 42
	if f.A() != 5 || f.B() != -7 || f.C() != 0x1abc || f.D != 42 {
		t.Fatalf("got a=%d b=%d c=%#x d=%d", f.A(), f.B(), f.C(), f.D)
	}
	raw := *(*uint32)(unsafe.Pointer(&f))
	want := uint32(5) | uint32(-7&0x1f)<<3 | uint32(0x1abc)<<8
	if raw != want {
		t.Fatalf("raw bits %#x, want %#x", raw, want)
	}
	f.SetA(0)
	if f.B() != -7 || f.C() != 0x1abc {
		t.Fatal("setting a clobbered b or c")
	}
	var h Holder
	h.X = 0x1122334455667788
	if *h.Y() != 0x55667788 {
		t.Fatalf("union y = %#x", *h.Y())
	}
	*h.Y() = 1
	if h.X != 0x1122334400000001 {
		t.Fatalf("union x = %#x", h.X)
	}
}
`
	os.WriteFile(dir+"/go.mod", []byte("module linux\n\ngo 1.22\n"), 0o644)
	os.WriteFile(dir+"/linux.go", out.Go, 0o644)
	os.WriteFile(dir+"/linux_test.go", []byte(check), 0o644)
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s\n--- generated:\n%s", err, b, out.Go)
	}
}
