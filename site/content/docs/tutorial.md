---
title: "Tutorial"
description: "Build, load and unload a \"hello world\" Linux kernel module written in Go, then add a /proc file and a module parameter."
weight: 2
---

This tutorial builds a tiny kernel module with gokm, loads it, and then
extends it with a `/proc` file and a module parameter. It takes about ten
minutes. The rest of the [documentation](/docs/) is the full reference.

> [!CAUTION]
> A kernel module runs with full privileges inside the kernel: a bug can
> freeze or crash the whole machine. Do this in a virtual machine you don't
> mind rebooting, not on your workstation.

## 1. What you need

- Linux x86-64
- Go and [TinyGo](https://tinygo.org/getting-started/install/) (tested with
  TinyGo 0.43.0; older releases don't work)
- The headers of the kernel you're running: `linux-headers` on Arch,
  `linux-headers-$(uname -r)` on Debian/Ubuntu, `kernel-devel` on Fedora
- `make` and binutils (`ld`, `objcopy`)
- Root access (`sudo`) to load modules

Check that everything is there:

```sh
go version
tinygo version
ls /lib/modules/$(uname -r)/build
```

If Secure Boot is enabled, the kernel only loads signed modules and
`insmod` fails with `Key was rejected by service`. A VM without Secure Boot
avoids that.

## 2. Install gokm

```sh
go install github.com/jespino/gokm@latest
```

Make sure `$(go env GOPATH)/bin` is in your `PATH`, then run `gokm` with no
arguments to see its commands.

## 3. Create a project

```sh
gokm init hello
cd hello
```

This writes three files:

- `go.mod`: the module name (`hello`) is also the kernel module's name.
- `main.go`: the module's code.
- `go.work`: lets your editor find gokm's packages. It contains paths for
  your machine only, so don't commit it.

## 4. Read the code

`main.go` is plain Go:

```go
package main

import "gokm/kernel"

func main() {
	kernel.Print("hello from Go!")

	kernel.OnExit(func() {
		kernel.Print("goodbye from Go!")
	})
}
```

- **`main`** runs when the module is loaded (`insmod`), as the module's init
  function.
- **`kernel.OnExit`** registers a function that runs when the module is
  unloaded (`rmmod`).
- **`kernel.Print`** writes a line to the kernel log, prefixed with the
  module's name. Read it with `dmesg`.

## 5. Build it

```sh
gokm build
```

This compiles the code with TinyGo, adds gokm's runtime, and links
`hello.ko` with the kernel's own build system. All intermediate files go
into `~/.cache/gokm`, and the first build takes longer while gokm prepares
them.

To build for a kernel other than the running one, pass `-kver <release>` or
`-kdir <kernel build dir>`. Add `-v` to see every command gokm runs.

## 6. Load and unload it

```sh
sudo insmod hello.ko
sudo dmesg | tail -1
# hello: hello from Go!

lsmod | grep hello
modinfo hello.ko

sudo rmmod hello
sudo dmesg | tail -1
# hello: goodbye from Go!
```

The kernel also logs that loading an out-of-tree module "taints" it. That's
expected for any module that isn't part of the kernel.

## 7. Add a /proc file and a parameter

Replace `main.go` with this:

```go
package main

import (
	"fmt"
	"sync/atomic"

	"gokm/kernel"
	"gokm/procfs"
)

//gokm:param who Who to greet in /proc/hello.
var who = "world"

var reads atomic.Int64

func main() {
	kernel.Print("hello from Go!")

	_, err := procfs.Create("hello", procfs.ReadFunc(func() ([]byte, error) {
		n := reads.Add(1)
		return []byte(fmt.Sprintf("Hello, %s! (read %d times)\n", who, n)), nil
	}))
	if err != nil {
		kernel.FailLoad(err)
		return
	}

	kernel.OnExit(func() {
		kernel.Print("goodbye from Go!")
	})
}
```

What's new:

- **`//gokm:param`** turns the package-level variable `who` into a module
  parameter. Its initial value is the default, and you can set another one
  when you load the module.
- **`procfs.Create`** creates `/proc/hello`. Each time a program opens the
  file, the function runs and what it returns becomes the file's content.
  gokm removes the file when the module is unloaded.
- **`sync/atomic`**: the kernel can run the function on several CPUs at
  once, so the counter must be safe for concurrent use.
- **`kernel.FailLoad`** makes `insmod` fail with that error, for example if
  `/proc/hello` already exists.

Rebuild, then load the module with a parameter:

```sh
gokm build
sudo insmod hello.ko who=gopher

cat /proc/hello
# Hello, gopher! (read 1 times)
cat /proc/hello
# Hello, gopher! (read 2 times)

cat /sys/module/hello/parameters/who
# gopher

sudo rmmod hello
```

## 8. Where to go next

- [Packages](/docs/packages/): filesystems ([gokm/fs](/docs/packages/fs/)),
  character devices and `/proc` files
  ([gokm/chardev and gokm/procfs](/docs/packages/chardev-procfs/)),
  network interfaces ([gokm/netdev](/docs/packages/netdev/)) and block
  devices ([gokm/blk](/docs/packages/blk/)).
- [gokm/linux](/docs/packages/linux/): call any kernel function directly.
- [Memory](/docs/memory/) and [Concurrency](/docs/concurrency/): how the Go
  heap and goroutines work inside the kernel.
- [Limitations](/docs/limitations/): what doesn't work yet.

The [examples](/docs/examples/) are complete modules to learn from: a
filesystem, character devices, network interfaces and a RAM disk.
