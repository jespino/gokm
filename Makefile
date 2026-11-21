# Developer targets for gokm itself. Users just need `go install .` (or
# `make`) and then `gokm init` / `gokm build`.
#
#   make                       build ./gokm
#   make test                  vet + unit tests
#   make qemu [KVER=...]       build the examples and test fixtures and run
#                              each in its own QEMU VM (KERNEL= picks the kernel
#                              image, default /boot/vmlinuz-linux; KVER must
#                              match it)
#   make site                  build the website (site/) into site/public/
#   make site-serve            preview the website at http://localhost:1313/gokm/

HUGO ?= hugo
KVER ?= $(shell file -bL $${KERNEL:-/boot/vmlinuz-linux} | sed -n 's/.*version \([^ ]*\).*/\1/p')

.PHONY: all gokm test qemu site site-api site-serve clean
all: gokm

gokm:
	go build -o gokm .

test:
	go vet ./...
	go test ./...

# qemu_test <name> <project dir> <build flags> <env for run.sh> <grep patterns...> [module params]:
# build the module, boot it in QEMU (log in _qemu/<name>.log) and require
# "TEST: PASS" plus every extra pattern in the log.
define qemu_test
	./gokm build -kver $(KVER) $(3) $(2)
	$(4) sh test/qemu/run.sh $(2)/$(notdir $(2)).ko $(6) >_qemu/$(1).log 2>&1 || true
	@for p in "TEST: PASS" $(5); do \
		grep -q "$$p" _qemu/$(1).log || { echo "FAIL: $(1): missing '$$p' (see _qemu/$(1).log)"; exit 1; }; \
	done
	@echo "PASS: $(1)"
endef

qemu: gokm
	mkdir -p _qemu
	$(call qemu_test,hello,examples/hello,,,"goodbye after")
	$(call qemu_test,gcstress,examples/gcstress,-heap-size 256K,,"gcstress: OK" "list still intact at exit")
	$(call qemu_test,gofs,examples/gofs,-heap-size 8M -heap-max 16M,TEST_PROG=examples/gofs/fstest,"fstest: ok   rename" "fstest: ok   symlinks" "fstest: ok   remount is a fresh" "fstest: ok   run a binary stored in gofs" "fstest: ok   xattrs")
	$(call qemu_test,devices,examples/devices,,TEST_PROG=examples/devices/devtest,"devtest: ok   ioctls" "devtest: ok   pipe: a blocked reader can be killed")
	$(call qemu_test,gonet,examples/gonet,,TEST_PROG=examples/gonet/nettest,"nettest: ok   UDP flood" "nettest: ok   /proc/goecho counters" "nettest: ok   goeth0: ping")
	$(call qemu_test,sysinfo,examples/sysinfo,,TEST_PROG=examples/sysinfo/infotest,"infotest: PASS")
	$(call qemu_test,gobrd,examples/gobrd,,TEST_PROG=examples/gobrd/brdtest EXTRA_MODULES="drivers/block/brd.ko.zst fs/nls/nls_iso8859-1.ko.zst fs/fat/fat.ko.zst fs/fat/vfat.ko.zst","brdtest: PASS" "unloaded. freed",rd_nr=2 rd_size=40960)
	$(call qemu_test,failload,test/fixtures/failload,,EXPECT_LOAD_ERRNO=ENODEV,)
	$(call qemu_test,nested,test/fixtures/nested,-heap-size 256K,,"nested: OK")
	$(call qemu_test,parked,test/fixtures/parked,-heap-size 256K,TEST_PROG=test/fixtures/parked/parkedtest,"parkedtest: PASS")
	$(call qemu_test,concurrency,test/fixtures/concurrency,-heap-size 256K -heap-max 64M,TEST_PROG=test/fixtures/concurrency/conctest,"conctest: ok   blocking in atomic context")
	@! grep -l "exit hook ran" _qemu/failload.log
	@! grep -lE "BUG:|Oops|WARNING:|Call Trace" _qemu/*.log
	@echo "all QEMU tests passed (logs in _qemu/)"

# The API reference pages are generated from the packages' doc comments.
site-api:
	cd site/apidoc && go run . -src ../../internal/assets/_files/lib -import gokm -out ../content/api

site: site-api
	$(HUGO) --source site --minify --gc

site-serve: site-api
	$(HUGO) server --source site

clean:
	rm -rf gokm examples/*/*.ko test/fixtures/*/*.ko _qemu site/public site/resources site/content/api
