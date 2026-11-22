<p align="center">
  <img src="assets/gopher-tux.png" alt="The Go gopher in a Tux costume" width="240">
</p>

# gokm — Go Kernel Modules

> [!WARNING]
> gokm is **experimental**. Don't use it in production: the modules it
> builds run inside the kernel, where a bug in the generated code or the Go
> runtime can crash the machine or corrupt data, and the API may change
> without notice.

`gokm` builds Linux kernel modules (x86-64) from plain Go code, using TinyGo.
A project is just Go files: filesystems, character devices, `/proc` files and
network devices are written against Go packages, and gokm supplies all the
kernel-side code. Goroutines, channels, `sync`, `fmt`, maps, `defer`/`recover`
and floating point all work, and Go code runs on all CPUs in parallel.

**Documentation: <https://jespino.github.io/gokm/>**. New to gokm? The
[tutorial](https://jespino.github.io/gokm/docs/tutorial/) walks you through building, loading and
extending a "hello world" kernel module in a few minutes.

```sh
gokm init hello          # creates hello/go.mod and hello/main.go
gokm build hello         # produces hello/hello.ko
sudo insmod hello/hello.ko && sudo rmmod hello && sudo dmesg | tail -2
# hello: hello from Go!
# hello: goodbye from Go!
```

```go
package main

import "gokm/kernel"

func main() { // runs at insmod
	kernel.Print("hello from Go!")
	kernel.OnExit(func() { kernel.Print("goodbye from Go!") }) // runs at rmmod
}
```

It's the same approach as [gopgext](https://github.com/jespino/gopgext)
(PostgreSQL extensions in Go): a custom TinyGo target and runtime layer for
the host, a C runtime compiled by the host's own build system (kbuild here,
PGXS there), and generated glue.

For persistent filesystems, `gokm/fs` provides directory/per-node sync, regular
hard links, persistent inode identity and lifecycle hooks, device flush/FUA
operations, and opt-in native sealed extent reads. See the
[storage API guide](site/content/docs/filesystem-storage.md) for contracts and
limitations; allocation and crash recovery remain filesystem responsibilities.

## Install

Requirements: Linux x86-64, Go, TinyGo (tested with 0.43.0), the kernel
headers for the target kernel (`linux-headers` on Arch; they include
`vmlinux` with BTF, used by `gokm/linux`), `make` and binutils (`ld`,
`objcopy`).

```sh
go install github.com/jespino/gokm@latest
```

See [Installation](https://jespino.github.io/gokm/docs/install/) for details.

## Documentation

| Page | What it covers |
|---|---|
| [Tutorial](https://jespino.github.io/gokm/docs/tutorial/) | a "hello world" module, then a `/proc` file and a module parameter |
| [Command line](https://jespino.github.io/gokm/docs/commands/) | `gokm init` and `gokm build`, and their flags |
| [Packages](https://jespino.github.io/gokm/docs/packages/) | `gokm/kernel`, `fs`, `chardev`, `procfs`, `netdev`, `blk`, module parameters and `gokm/linux` |
| [API reference](https://jespino.github.io/gokm/api/) | every exported identifier of the Go packages |
| [Memory](https://jespino.github.io/gokm/docs/memory/) | the garbage-collected Go heap inside the kernel |
| [Concurrency](https://jespino.github.io/gokm/docs/concurrency/) | parallel Go, goroutines as kernel threads, atomic context |
| [Examples](https://jespino.github.io/gokm/docs/examples/) | complete modules: a filesystem, devices, network interfaces, a RAM disk |
| [How it works](https://jespino.github.io/gokm/docs/internals/) | the build pipeline, the TinyGo target and the runtime |
| [Limitations](https://jespino.github.io/gokm/docs/limitations/) | what isn't supported yet |

The website's source is in [`site/`](site/): `make site-serve` previews it
locally.

## Development

```sh
make test     # go vet + unit tests (BTF parsing and binding generation use the running kernel's BTF)
make qemu     # build every example and fixture and run each in a QEMU VM with its test program
```

## License

gokm is released under the [MIT License](LICENSE), except:

- The C code that gokm compiles into every module (the runtime in
  `internal/assets/_files/kmod/`, the framework glue in
  `internal/assets/_files/frameworks/`, and the C files gokm generates) is
  under the [GPL-2.0](LICENSES/GPL-2.0.txt), as its
  `SPDX-License-Identifier` headers say. It uses GPL-only kernel symbols,
  which is also why modules built with gokm need a GPL-compatible
  `MODULE_LICENSE` (`-license`).
- `examples/gobrd/main.go` is a port of Linux's `drivers/block/brd.c` and is
  under the GPL-2.0.

## Mascot

The gokm mascot is the Go gopher dressed as Tux, the Linux penguin
created by Larry Ewing. It's based on the Go gopher designed by
[Renée French](https://reneefrench.blogspot.com/), licensed under
[CC BY 4.0](https://creativecommons.org/licenses/by/4.0/).
