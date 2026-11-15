// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "internal/stringslite"

// Simulation support for GOEXPERIMENT=detsched on darwin; see detsim.go.
//
// On darwin, packages syscall and golang.org/x/sys/unix make every system
// call through libc: syscall_syscalln and syscall_rawsyscalln call a
// trampoline that jumps to a libc function. Those offer each call to the
// simulation, as syscall.Syscall does on Linux, with the trampoline's
// address in place of the system call number. Each package has its own
// trampolines; the simulation tells them apart by the trampoline's name
// (libc_read_trampoline), which it looks up once per address. fcntl,
// which internal/syscall/unix.Fcntl calls directly, offers its calls too,
// with its own trampoline (fcntl_trampoline).

// detsimLibcCall offers a libc call with the given trampoline and
// arguments, the first six of them, to the simulation, which reports
// whether it handled the call. raw says the caller is
// syscall_rawsyscalln, which also runs in the child of fork, so it offers
// only the calls that package syscall makes raw and the simulation
// handles (detsimRawLibc).
// It must not grow the stack: the arguments may point into it.
//
//go:nosplit
func detsimLibcCall(fn uintptr, args []uintptr, raw bool) (r1, r2, err uintptr, ok bool) {
	if detsim.handler == nil {
		if len(args) > 0 && detsimFD(args[0]) {
			return detsimClosedDir(fn)
		}
		return
	}
	gp := getg()
	if gp != gp.m.curg || raw && gp.syscallsp != 0 {
		// On the system stack, or in syscall_syscalln, which offered the
		// call already.
		return
	}
	var a [6]uintptr
	copy(a[:], args)
	node := detG(gp).node
	// A goroutine of no node offers only socket calls, which the
	// simulation refuses it while a world is active, and calls on the
	// simulation's descriptors and mappings, as on Linux (see
	// syscall_detsimSyscall).
	noNode := node == 0 && !detsimFD(a[0]) && !detsimFD(a[2]) && !detsimMapAddr(a[0])
	systemstack(func() {
		if raw && !detsimRawLibc(fn) || noNode && detsimLibcName(fn) != "socket" {
			return
		}
		r1, r2, err, ok = detsim.handler(node, fn, a)
	})
	if detG(gp).wait != 0 {
		detsimWaitDevice(gp)
	}
	return
}

// detsimRawLibc reports whether fn is the trampoline of a call that package
// syscall (or golang.org/x/sys/unix) makes with rawSyscall and the
// simulation handles: socket calls, and getpid, which gives each simulated
// process its own ID.
func detsimRawLibc(fn uintptr) bool {
	switch detsimLibcName(fn) {
	case "socket", "socketpair", "getsockname", "getpeername", "getpid":
		return true
	}
	return false
}

// detsimClosedDir fails the readdir_r or closedir call fn on a simulated
// directory stream after its simulation ended. The simulation's DIR* is a
// descriptor number, which libc would dereference.
//
//go:nosplit
func detsimClosedDir(fn uintptr) (r1, r2, err uintptr, ok bool) {
	const ebadf = 9
	var name string
	systemstack(func() { name = detsimLibcName(fn) })
	switch name {
	case "readdir_r":
		return ebadf, 0, 0, true // readdir_r returns the error
	case "closedir":
		return ^uintptr(0), 0, ebadf, true
	}
	return
}

// detsimLibcName returns the libc function of the trampoline fn, such as
// "read" for syscall.libc_read_trampoline, or "".
func detsimLibcName(fn uintptr) string {
	f := findfunc(fn)
	if !f.valid() {
		return ""
	}
	name := funcname(f)
	i := stringslite.Index(name, ".libc_")
	if i < 0 || !stringslite.HasSuffix(name, "_trampoline") {
		return ""
	}
	return name[i+len(".libc_") : len(name)-len("_trampoline")]
}
