// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// detThreadSleeping reports whether mp's thread sleeps in the kernel
// waiting for an event, state S in its /proc stat, rather than running,
// waiting for a CPU, or waiting for a disk. Only a sleeping system call
// can be waiting for something that another goroutine must do first, so
// only one detRetakeSyscall hands off the P for (see detsched.go); a
// thread that a loaded machine has not given a CPU for 10ms is not. If
// the state cannot be read, it reports true, as the watchdog then hands
// off as before.
func detThreadSleeping(mp *m) bool {
	var path [64]byte
	n := copy(path[:], "/proc/self/task/")
	var d [20]byte
	i := len(d)
	for v := mp.procid; ; v /= 10 {
		i--
		d[i] = byte('0' + v%10)
		if v < 10 {
			break
		}
	}
	n += copy(path[n:], d[i:])
	copy(path[n:], "/stat\x00")
	fd := open(&path[0], 0 /* O_RDONLY */, 0)
	if fd < 0 {
		return true
	}
	var buf [512]byte
	r := read(fd, noescape(unsafe.Pointer(&buf[0])), int32(len(buf)))
	closefd(fd)
	// The state follows the command name in parentheses, which may
	// itself hold parentheses: it is after the last ") ".
	for j := r - 2; j > 0; j-- {
		if buf[j-1] == ')' && buf[j] == ' ' {
			return buf[j+1] == 'S'
		}
	}
	return true
}
