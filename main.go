// Command gokm (Go Kernel Module) builds Linux kernel modules from Go code
// using TinyGo.
//
//	gokm init [dir]     create a new module project
//	gokm build [dir]    compile the project into <name>.ko
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/jespino/gokm/internal/build"
	"github.com/jespino/tinyguest/cli"
	"github.com/jespino/tinyguest/project"
)

const version = "0.1.0"

func usage() {
	fmt.Fprint(os.Stderr, `gokm - build Linux kernel modules from Go code (with TinyGo)

Usage:
  gokm init [-module path] [dir]   create a new module project in dir (default .)
  gokm build [flags] [dir]         compile the project in dir (default .) into a .ko
  gokm version                     print the gokm version

Run "gokm <command> -h" for the command's flags.
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit(os.Args[2:])
	case "build":
		err = cmdBuild(os.Args[2:])
	case "version", "-version", "--version":
		fmt.Println("gokm", version)
	case "help", "-h", "-help", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "gokm: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "gokm:", err)
		}
		os.Exit(1)
	}
}

func cmdBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	var o build.Options
	fs.StringVar(&o.Output, "o", "", "output `file` (default <dir>/<name>.ko)")
	fs.StringVar(&o.Name, "name", "", "kernel module `name` (default: last element of the go.mod module path)")
	fs.StringVar(&o.KernelVer, "kver", "", "kernel `release` to build for (default: uname -r)")
	fs.StringVar(&o.KernelDir, "kdir", "", "kernel build `dir` (default: /lib/modules/<kver>/build)")
	fs.StringVar(&o.Description, "description", "", "MODULE_DESCRIPTION `text`")
	fs.StringVar(&o.Author, "author", "", "MODULE_AUTHOR `text`")
	fs.StringVar(&o.License, "license", "GPL", "MODULE_LICENSE: one of "+strings.Join(build.Licenses, ", ")+"\n(GPL-compatible only: gokm's runtime uses GPL-only kernel symbols)")
	fs.StringVar(&o.HeapSize, "heap-size", build.DefaultHeapSize, "initial `size` of the garbage-collected Go heap, e.g. 512K, 4M\n(can be overridden at load time: insmod mod.ko heap_size=8M)")
	fs.StringVar(&o.HeapMax, "heap-max", build.DefaultHeapMax, "`size` limit the Go heap can grow to (load time: heap_max=...)")
	fs.StringVar(&o.StackLimit, "stack-limit", "8K", "warn about Go call paths using more kernel stack than `size` (0 disables the analysis)")
	fs.BoolVar(&o.Verbose, "v", false, "print the commands run and their output")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: gokm build [flags] [dir]")
		fs.PrintDefaults()
	}
	if err := cli.ParseInterspersed(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errors.New("build takes at most one directory")
	}
	o.Dir = "."
	if fs.NArg() == 1 {
		o.Dir = fs.Arg(0)
	}
	res, err := build.Build(o)
	if err != nil {
		return err
	}
	rel := res.Output
	if wd, err := os.Getwd(); err == nil {
		if r, err := filepath.Rel(wd, res.Output); err == nil && !strings.HasPrefix(r, "..") {
			rel = r
		}
	}
	fmt.Fprintf(os.Stderr, "gokm: built %s (module %q)\n", rel, res.Name)
	if o.Verbose {
		fmt.Fprintf(os.Stderr, "gokm: work dir %s\n", res.WorkDir)
	}
	return nil
}

var mainTmpl = template.Must(template.New("main").Parse(`// {{.Name}} is a Linux kernel module written in Go, built with gokm.
//
// main runs when the module is loaded (insmod); the function registered with
// kernel.OnExit runs when it's unloaded (rmmod). Messages printed with
// kernel.Print go to the kernel log (dmesg).
package main

import "gokm/kernel"

func main() {
	kernel.Print("hello from Go!")

	kernel.OnExit(func() {
		kernel.Print("goodbye from Go!")
	})
}
`))

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	modPath := fs.String("module", "", "Go module `path` (default: the directory name)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: gokm init [-module path] [dir]")
		fs.PrintDefaults()
	}
	if err := cli.ParseInterspersed(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errors.New("init takes at most one directory")
	}
	dir := "."
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if *modPath == "" {
		*modPath = filepath.Base(abs)
	}
	name := build.ModuleNameFromPath(*modPath)
	if err := build.ValidateModuleName(name); err != nil {
		return fmt.Errorf("%w (derived from module path %q; use -module)", err, *modPath)
	}

	goVersion := "1.22"
	var main strings.Builder
	if err := mainTmpl.Execute(&main, map[string]string{"Name": name}); err != nil {
		return err
	}
	if err := project.Create(abs, map[string][]byte{
		"go.mod":  project.GoModFile(*modPath, goVersion),
		"main.go": []byte(main.String()),
	}); err != nil {
		return err
	}

	// Editor support: a go.work that makes "gokm/kernel" resolvable. Optional,
	// so failures here (e.g. TinyGo not installed yet) are only warnings.
	if root, err := build.PrepareAssets(); err != nil {
		fmt.Fprintf(os.Stderr, "gokm: warning: skipping go.work for editor support: %v\n", err)
	} else {
		work := build.Workspace.Editor(goVersion, root.Lib)
		if err := os.WriteFile(filepath.Join(abs, "go.work"), []byte(work), 0o644); err != nil {
			return err
		}
	}

	fmt.Fprintf(os.Stderr, "gokm: created kernel module %q in %s\n", name, abs)
	fmt.Fprintf(os.Stderr, "  build:  gokm build %s\n", dir)
	fmt.Fprintf(os.Stderr, "  load:   sudo insmod %s\n", filepath.Join(dir, name+".ko"))
	fmt.Fprintf(os.Stderr, "  unload: sudo rmmod %s\n", name)
	return nil
}
