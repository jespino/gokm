---
title: "Installation"
description: "What you need to build kernel modules with gokm, and how to install it."
weight: 1
---

## Requirements

- Linux x86-64
- Go
- [TinyGo](https://tinygo.org/getting-started/install/), tested with 0.43.0.
  Older releases don't work.
- The headers of the kernel you build for (`linux-headers` on Arch,
  `linux-headers-$(uname -r)` on Debian/Ubuntu, `kernel-devel` on Fedora).
  They include `vmlinux` with BTF, used by `gokm/linux`.
- `make` and binutils (`ld`, `objcopy`)

## Install gokm

```sh
go install github.com/jespino/gokm@latest
```

`go install` puts `gokm` in `$(go env GOPATH)/bin`; make sure it's in your
`PATH`. Then follow the [tutorial](/docs/tutorial/).

To build from source instead:

```sh
git clone https://github.com/jespino/gokm
cd gokm
go install .          # or: make  (builds ./gokm)
```

> [!CAUTION]
> A kernel module runs with full privileges inside the kernel: a bug can
> freeze or crash the whole machine. Try your modules in a virtual machine.
