// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && (amd64 || arm64)

package machine

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// filesSum reads in its init what describes the machine, as
// go.uber.org/automaxprocs and memory-limit packages do: it finds the
// process's cgroup v2 directory from /proc/self/cgroup and
// /proc/self/mountinfo and reads its cpu.max and memory.max, falls back
// to cgroup v1's files, counts the processors in /proc/cpuinfo and
// /proc/stat and the CPUs online and in its affinity, and reads the
// memory size from /proc/meminfo and sysinfo. Parsing counts ticks by
// what it reads. With MACHINE_FILES set it prints what it found.
var filesSum = func() int {
	n := 0
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			return "error: " + err.Error()
		}
		for _, c := range b {
			if c == '\n' {
				n++
			}
		}
		return string(b)
	}
	field := func(text, key string) string {
		s := bufio.NewScanner(strings.NewReader(text))
		for s.Scan() {
			if k, v, ok := strings.Cut(s.Text(), ":"); ok && strings.TrimSpace(k) == key {
				return strings.TrimSpace(v)
			}
		}
		return ""
	}

	// automaxprocs: the cgroup v2 path, its mount point, and cpu.max.
	cgroupPath := ""
	for _, line := range strings.Split(read("/proc/self/cgroup"), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			cgroupPath = p
		}
	}
	mount := ""
	for _, line := range strings.Split(read("/proc/self/mountinfo"), "\n") {
		f := strings.Fields(line)
		for i, x := range f {
			if x == "-" && i+1 < len(f) && f[i+1] == "cgroup2" {
				mount = f[4]
			}
		}
	}
	dir := path.Join(mount, cgroupPath)
	cpuMax := strings.TrimSpace(read(dir + "/cpu.max"))
	quota := -1
	if q, _, ok := strings.Cut(cpuMax, " "); ok && q != "max" {
		quota, _ = strconv.Atoi(q)
	}
	memMax := strings.TrimSpace(read(dir + "/memory.max"))
	v1Quota := strings.TrimSpace(read("/sys/fs/cgroup/cpu,cpuacct/cpu.cfs_quota_us"))
	v1Mem := strings.TrimSpace(read("/sys/fs/cgroup/memory/memory.limit_in_bytes"))

	// CPUs.
	cpuinfo := read("/proc/cpuinfo")
	processors := strings.Count(cpuinfo, "processor\t:")
	stat := 0
	for _, line := range strings.Split(read("/proc/stat"), "\n") {
		if strings.HasPrefix(line, "cpu") && !strings.HasPrefix(line, "cpu ") {
			stat++
		}
	}
	online := strings.TrimSpace(read("/sys/devices/system/cpu/online"))
	var set [16]uint64
	syscall.RawSyscall(syscall.SYS_SCHED_GETAFFINITY, 0, unsafe.Sizeof(set), uintptr(unsafe.Pointer(&set)))
	affinity := 0
	for _, w := range set {
		for ; w != 0; w &= w - 1 {
			affinity++
		}
	}

	// Memory.
	memTotal, _ := strconv.Atoi(strings.TrimSuffix(field(read("/proc/meminfo"), "MemTotal"), " kB"))
	var si syscall.Sysinfo_t
	syscall.Sysinfo(&si)
	sysinfoTotal := uint64(si.Totalram) * uint64(si.Unit)

	for i := range processors*stat + affinity + len(cpuMax) + len(memMax) + len(online) + memTotal%97 + int(sysinfoTotal%89) + quota + 1 {
		n += leaf(i)
	}
	n += bytes.Count([]byte(cpuinfo), []byte(" "))
	if os.Getenv("MACHINE_FILES") != "" {
		fmt.Printf("files: cgroup %s mount %s cpu.max %q memory.max %q v1 %q %q\n", cgroupPath, mount, cpuMax, memMax, v1Quota, v1Mem)
		fmt.Printf("files: processors %d stat %d online %s affinity %d memtotal %d sysinfo %d\n", processors, stat, online, affinity, memTotal, sysinfoTotal)
	}
	return n
}()

// hostSum reads what a container's runtime and kernel give each
// container, as code does after init: the host name, the kernel's release,
// /etc/resolv.conf and /etc/hosts through a lookup of localhost, a
// directory's entries in the order the file system gives them, stopping
// at the first that matches as code that looks for one does, and a copy
// of one file to another, which the kernel can do on some file systems.
// With MACHINE_FILES set it prints what it found.
var hostSum = func() int {
	n := 0
	host, _ := os.Hostname()
	var u syscall.Utsname
	syscall.Uname(&u)
	release := ""
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		release += string(rune(c))
	}
	addrs, err := net.LookupHost("localhost")
	if err != nil {
		addrs = []string{err.Error()}
	}
	dir, err := os.MkdirTemp("", "machine")
	first := ""
	copied := int64(0)
	if err == nil {
		defer os.RemoveAll(dir)
		for i := range 40 {
			os.WriteFile(filepath.Join(dir, "entry"+strconv.Itoa(i*37%41)), make([]byte, i*100), 0o644)
		}
		if f, err := os.Open(dir); err == nil {
			names, _ := f.Readdirnames(-1)
			f.Close()
			for _, name := range names {
				n += len(name)
				if strings.HasSuffix(name, "7") {
					first = name
					break
				}
			}
		}
		src, err1 := os.Open(filepath.Join(dir, "entry"+strconv.Itoa(39*37%41)))
		dst, err2 := os.Create(filepath.Join(dir, "copy"))
		if err1 == nil && err2 == nil {
			copied, _ = io.Copy(dst, src)
		}
		src.Close()
		dst.Close()
	}
	for i := range len(host) + len(release) + len(strings.Join(addrs, " ")) {
		n += leaf(i)
	}
	if os.Getenv("MACHINE_FILES") != "" {
		fmt.Printf("host: %s release %s localhost %v first %s copied %d\n", host, release, addrs, first, copied)
	}
	return n
}()
