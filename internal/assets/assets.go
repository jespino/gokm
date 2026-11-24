// Package assets holds the support files gokm needs to turn Go code into a
// kernel module, and materializes them in the user's cache directory (with
// tinyguest/assets).
//
// _files/ layout:
//
//	target.json   TinyGo target for code running inside the Linux kernel
//	overlay/      files added on top of TinyGo's source tree (runtime platform)
//	lib/          the "gokm" Go module: gokm/kernel, gokm/fs, gokm/netdev, ...
//	kmod/         C shim compiled into every module by kbuild
//	frameworks/   C glue for framework packages: frameworks/fs/ is compiled
//	              in when the project imports gokm/fs
//
// The directory name starts with "_" so the go tool doesn't treat the Go files
// in it (which only build with TinyGo for the kernel target) as packages of
// this module.
package assets

import (
	"embed"
	"io/fs"
	"path/filepath"

	tgassets "github.com/jespino/tinyguest/assets"
	"github.com/jespino/tinyguest/toolchain"
)

//go:embed _files
var files embed.FS

// LibModule is the module path of the Go package user code imports.
const LibModule = "gokm"

// LibGoVersion is the go directive of the generated lib go.mod.
const LibGoVersion = "1.22"

// Platform is the build tag of the runtime layer in overlay/.
const Platform = "linuxkernel"

// excludedFiles are TinyGo source files (relative to the root) that would
// otherwise build for our target but conflict with the linuxkernel runtime
// layer: baremetal_memory.go takes heap and stack bounds from linker-script
// symbols, gc_custom.go and task_threads.go are replaced by gokm's
// collector and kernel-thread goroutines.
var excludedFiles = []string{
	"src/runtime/baremetal_memory.go",
	"src/runtime/gc_custom.go",          // gokm's collector: gc_linuxkernel.go
	"src/internal/task/task_threads.go", // kernel threads: task_linuxkernel.go
}

// Root is a materialized copy of the assets plus a TinyGo root overlay.
type Root struct {
	*tgassets.Root
	Kmod       string // C shim sources
	Frameworks string // C glue of framework packages: frameworks/<pkg>/*.c
}

// Prepare materializes the assets for the given TinyGo under cacheDir,
// reusing a previous copy when nothing changed.
func Prepare(cacheDir string, tg *toolchain.TinyGo) (*Root, error) {
	sub, err := fs.Sub(files, "_files")
	if err != nil {
		return nil, err
	}
	r, err := tgassets.Prepare(tgassets.Config{
		Files:        sub,
		Platform:     Platform,
		Exclude:      excludedFiles,
		LibModule:    LibModule,
		LibGoVersion: LibGoVersion,
		CacheDir:     cacheDir,
		TinyGo:       tg,
	})
	if err != nil {
		return nil, err
	}
	return &Root{
		Root:       r,
		Kmod:       filepath.Join(r.Dir, "kmod"),
		Frameworks: filepath.Join(r.Dir, "frameworks"),
	}, nil
}

// File returns the content of an embedded file (path relative to _files).
func File(name string) ([]byte, error) {
	return files.ReadFile("_files/" + name)
}
