// Command infotest checks /proc/gosysinfo inside the QEMU VM (-smp 2).
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func field(info, key string) string {
	for _, l := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(l, key+": "); ok {
			return v
		}
	}
	return ""
}

func main() {
	b, err := os.ReadFile("/proc/gosysinfo")
	if err != nil {
		fmt.Println("infotest: FAIL read:", err)
		os.Exit(1)
	}
	info := string(b)
	fmt.Printf("infotest: %s\n", strings.ReplaceAll(strings.TrimSpace(info), "\n", ", "))

	var uts syscall.Utsname
	syscall.Uname(&uts)
	var rel []byte
	for _, c := range uts.Release {
		if c == 0 {
			break
		}
		rel = append(rel, byte(c))
	}
	total, _ := strconv.Atoi(field(info, "ram_total_mib"))
	var problems []string
	if field(info, "release") != string(rel) {
		problems = append(problems, "release != uname -r ("+string(rel)+")")
	}
	if field(info, "machine") != "x86_64" {
		problems = append(problems, "machine")
	}
	if field(info, "cpus") != "2" {
		problems = append(problems, "cpus != 2")
	}
	var si syscall.Sysinfo_t
	syscall.Sysinfo(&si)
	if want := int(uint64(si.Totalram) * uint64(si.Unit) >> 20); total != want {
		problems = append(problems, fmt.Sprintf("ram_total_mib != sysinfo(2) (%d)", want))
	}
	if field(info, "timespec") != "1.500000000" {
		problems = append(problems, "timespec (by-value struct return)")
	}
	if field(info, "page_size") != "4096" {
		problems = append(problems, "page_size")
	}

	// Blocking call: writing "300" sleeps 300 ms in msleep with the Go lock
	// released, so a concurrent read completes long before it.
	start := time.Now()
	done := make(chan time.Duration)
	go func() {
		if err := os.WriteFile("/proc/gosysinfo", []byte("300\n"), 0); err != nil {
			fmt.Println("infotest: write:", err)
		}
		done <- time.Since(start)
	}()
	time.Sleep(30 * time.Millisecond)
	os.ReadFile("/proc/gosysinfo")
	readAt := time.Since(start)
	sleptAt := <-done
	fmt.Printf("infotest: concurrent read done at %v, msleep(300) write done at %v\n",
		readAt.Round(time.Millisecond), sleptAt.Round(time.Millisecond))
	if sleptAt < 300*time.Millisecond || readAt > 200*time.Millisecond {
		problems = append(problems, "blocking call didn't release the Go lock")
	}
	if err := os.WriteFile("/proc/gosysinfo", []byte("not-a-number"), 0); err == nil {
		problems = append(problems, "kstrtouint accepted garbage")
	}

	if len(problems) > 0 {
		fmt.Println("infotest: FAIL:", strings.Join(problems, "; "))
		os.Exit(1)
	}
	fmt.Println("infotest: PASS")
}
