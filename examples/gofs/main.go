// gofs is an in-memory filesystem written in pure Go with gokm/fs: the tree,
// the file contents and all checks live in fs.go; gokm provides the kernel
// side.
//
//	gokm build -heap-size 8M
//	sudo insmod gofs.ko
//	sudo mount -t gofs none /mnt
package main

import (
	"gokm/fs"
	"gokm/kernel"
)

func main() {
	err := fs.Register(&fs.FileSystem{
		Name:      "gofs",
		Magic:     0x676f6673, // "gofs"
		MaxIOSize: 1 << 20,
		Mount:     func() (fs.Dir, error) { return newDir(0o755), nil },
	})
	if err != nil {
		kernel.FailLoad(err)
		return
	}
	kernel.Print("registered filesystem type \"gofs\"")
}
