// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// controlSockets returns a SOCK_SEQPACKET Unix socket pair, for a fork
// server's control socket: each command and reply is one message.
func controlSockets() ([2]int, error) {
	return syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
}

// memory returns how many bytes of memory are available to programs, and
// how many there are, or zeros if it cannot tell. The memory limits of
// detsched's cgroup and its parents, as in a container, count too.
func memory() (avail, total uint64) {
	b, _ := os.ReadFile("/proc/meminfo")
	for line := range strings.Lines(string(b)) {
		k, v, _ := strings.Cut(line, ":")
		n, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		switch k {
		case "MemAvailable":
			avail = n << 10
		case "MemTotal":
			total = n << 10
		}
	}
	b, _ = os.ReadFile("/proc/self/cgroup")
	if dir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "0::"); ok {
		for ; dir != "/" && dir != "." && dir != ""; dir = filepath.Dir(dir) {
			max, err1 := readUint("/sys/fs/cgroup" + dir + "/memory.max")
			cur, err2 := readUint("/sys/fs/cgroup" + dir + "/memory.current")
			if err1 == nil && err2 == nil {
				// The page cache counts in memory.current, but the
				// kernel reclaims it before it runs out, as
				// MemAvailable assumes; shared memory it can't.
				cache := statField(dir, "file") - min(statField(dir, "shmem"), statField(dir, "file"))
				cur -= min(cache, cur)
				total = min(total, max)
				avail = min(avail, max-min(cur, max))
			}
		}
	}
	return avail, total
}

// statField returns a field of the cgroup's memory.stat, or 0.
func statField(dir, key string) uint64 {
	b, _ := os.ReadFile("/sys/fs/cgroup" + dir + "/memory.stat")
	for line := range strings.Lines(string(b)) {
		if v, ok := strings.CutPrefix(line, key+" "); ok {
			n, _ := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			return n
		}
	}
	return 0
}

func readUint(file string) (uint64, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64) // fails for "max"
}

// lowerPriority lowers our scheduling priority to nice 10, which the
// programs we run inherit, so that a search that keeps every CPU busy
// leaves the CPUs to the user's other work when it wants them. Linux keeps
// a nice value per thread, so it sets each of ours, twice in case one was
// started meanwhile; threads started later inherit it.
func lowerPriority() {
	for range 2 {
		tasks, _ := os.ReadDir("/proc/self/task")
		for _, t := range tasks {
			if tid, err := strconv.Atoi(t.Name()); err == nil {
				syscall.Setpriority(syscall.PRIO_PROCESS, tid, 10)
			}
		}
	}
}
