package assets

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Run the actual framework Go code on the host, replacing only foreign C
// declarations and the kernel runtime boundary. Kernel/VFS behavior is covered
// separately by gofs/fstest and the fsblock QEMU fixture.
func TestFilesystemCallbacks(t *testing.T) {
	dir := t.TempDir()
	write := func(name, source string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(source), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module gokm\n\ngo 1.24\n")
	entries, err := files.ReadDir("_files/lib/fs")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		b, err := File("lib/fs/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		source := string(b)
		set := token.NewFileSet()
		f, err := parser.ParseFile(set, entry.Name(), source, 0)
		if err != nil {
			t.Fatal(err)
		}
		for i := len(f.Decls) - 1; i >= 0; i-- {
			fn, ok := f.Decls[i].(*ast.FuncDecl)
			if !ok || fn.Body != nil {
				continue
			}
			body := " { panic(\"unexpected foreign call: " + fn.Name.Name + "\") }"
			switch fn.Name.Name {
			case "cDevRead", "cDevWrite", "cDevSetBlockSize", "cInstallMapping":
				body = " { foreignCalls++; return foreignResult }"
			case "cDevSync":
				body = " { foreignCalls++; lastFlush = flush; return foreignResult }"
			case "cDevIO":
				body = " { foreignCalls++; lastFlags = flags; return foreignResult }"
			case "cEmit":
				body = " { emitted++; return true }"
			case "cEmitCookie":
				body = " { emitted++; lastCookie=next; return true }"
			}
			end := set.Position(fn.End()).Offset
			source = source[:end] + body + source[end:]
		}
		write("fs/"+entry.Name(), source)
	}
	for _, name := range []string{"errno.go", "handle.go", "panic.go"} {
		b, err := File("lib/kernel/" + name)
		if err != nil {
			t.Fatal(err)
		}
		source := string(b)
		if name == "errno.go" {
			source = strings.ReplaceAll(source, "//go:linkname setInitError runtime.kmodSetInitError", "")
			source = strings.ReplaceAll(source, "func setInitError(errno int)", "func setInitError(errno int) {}")
		}
		write("kernel/"+name, source)
	}
	write("kernel/host.go", "package kernel\nfunc Print(string) {}\nfunc OnExit(func()) {}\n")
	b, err := os.ReadFile("testdata/fs_callbacks_test.go")
	if err != nil {
		t.Fatal(err)
	}
	write("fs/callbacks_test.go", string(b))
	cmd := exec.Command("go", "test", "-race", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-buildvcs=false")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("host fs callbacks: %v\n%s", err, out)
	}
}
