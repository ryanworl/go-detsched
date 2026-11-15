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
				total = min(total, max)
				avail = min(avail, max-min(cur, max))
			}
		}
	}
	return avail, total
}

func readUint(file string) (uint64, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64) // fails for "max"
}
