// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin && !ios && arm64

package runtime

import (
	"internal/abi"
	"unsafe"
)

// The system calls of fork mode on darwin; see detfork.go. They go
// through libc, as all system calls do on darwin.
//
// fork(2) runs libSystem's atfork handlers, which make libc usable in the
// child: its malloc zones, its thread list, and its mach ports. The
// runtime itself keeps a little state that names threads the child
// doesn't have: each M's pthread mutex and condition variable, which a
// thread of the parent may have held, its park note, which records that
// it was waiting, and the stack bounds of its pthread. detForkResetM
// clears them, and newosproc gives the M a new pthread.
//
// Go reaches Objective-C and Security.framework only in crypto/x509, when
// it verifies a certificate with the system's verifier
// (crypto_x509_syscall). A child may do that: det mode runs with
// Objective-C's fork safety check off (see detaslr_darwin.go). A process
// that did it cannot fork, though, because Security.framework's
// connection to trustd does not survive fork and a child that verifies
// again crashes; detForkCheck refuses with detForkSecurity.

//go:cgo_import_dynamic libc_fork fork "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_wait4 wait4 "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_dup2 dup2 "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_recvmsg recvmsg "/usr/lib/libSystem.B.dylib"

// detForkCall holds the arguments and results of the trampolines in
// detfork_darwin_arm64.s.
type detForkCall struct {
	a1, a2, a3 uintptr
	r, errno   uintptr
}

func detfork_fork_trampoline()
func detfork_wait4_trampoline()
func detfork_dup2_trampoline()
func detfork_recvmsg_trampoline()

// detForkFork forks the process. It returns the child's PID in the
// parent, and 0 in the child.
func detForkFork() (r, errno uintptr) {
	var c detForkCall
	libcCall(unsafe.Pointer(abi.FuncPCABI0(detfork_fork_trampoline)), unsafe.Pointer(&c))
	return c.r, c.errno
}

// detForkSelf records the pthread of the child's only thread and gives
// it its signal stack again: unlike Linux, macOS does not keep the
// forking thread's alternate signal stack in the child. Without it a
// signal, such as the SIGSEGV of a nil dereference that the program
// recovers from, is handled on the goroutine's stack, where the handler
// finds no signal stack it knows and waits for good for an M that a
// program without cgo never has.
func detForkSelf() {
	getg().m.procid = uint64(pthread_self())
	minitSignalStack()
}

// detForkResetM resets the runtime's per-thread state of mp, an M in a
// child: its mutex and condition variable, which a thread of the parent
// that no longer exists may hold; its semaphore count, unless mp is m0,
// which forked; for an idle M, its park note, which still records that
// the M waits on it; its g0 stack, which was its pthread's, so that
// mstart takes the bounds of the new pthread's stack; and the libc call
// it was parked in, which it never returns from.
func detForkResetM(mp *m) {
	if pthread_mutex_init(&mp.mutex, nil) != 0 || pthread_cond_init(&mp.cond, nil) != 0 {
		throw("detfork: cannot reset an M's semaphore")
	}
	if mp == &m0 {
		return
	}
	mp.count = 0
	if mp.park.key == uintptr(unsafe.Pointer(mp)) {
		mp.park.key = 0
	}
	mp.g0.stack.lo, mp.g0.stack.hi = 0, 0
	mp.libcallsp, mp.libcallpc, mp.libcallg = 0, 0, 0
}

// detForkPollReinit gives a child a new kqueue, which fork doesn't
// inherit.
func detForkPollReinit() {
	closefd(kq)
	netpollWakeSig.Store(0)
	netpollinit()
}

func detForkDup2(oldfd, newfd int32) {
	c := detForkCall{a1: uintptr(oldfd), a2: uintptr(newfd)}
	libcCall(unsafe.Pointer(abi.FuncPCABI0(detfork_dup2_trampoline)), unsafe.Pointer(&c))
}

// detForkWait4 waits for the child pid (-1: any child) with the given
// options and returns its PID, or 0 with WNOHANG if none exited.
func detForkWait4(pid int64, status *int32, options uintptr) (r, errno uintptr) {
	c := detForkCall{a1: uintptr(pid), a2: uintptr(unsafe.Pointer(status)), a3: options}
	libcCall(unsafe.Pointer(abi.FuncPCABI0(detfork_wait4_trampoline)), unsafe.Pointer(&c))
	KeepAlive(status)
	return c.r, c.errno
}

// detForkRecv receives a command, with up to 5 descriptors, from the
// driver.
func detForkRecv(cmd *[4]uint64, fds *[5]int32) (int, bool) {
	type iovec struct {
		base *byte
		len  uint64
	}
	type msghdr struct {
		name       *byte
		namelen    uint32
		iov        *iovec
		iovlen     int32
		control    *byte
		controllen uint32
		flags      int32
	}
	var control [12 + 4*5]byte // cmsghdr (len uint32, level, type int32), then the fds
	iov := iovec{(*byte)(unsafe.Pointer(cmd)), uint64(unsafe.Sizeof(*cmd))}
	msg := msghdr{iov: &iov, iovlen: 1, control: &control[0], controllen: uint32(len(control))}
	for {
		c := detForkCall{a1: uintptr(detFork.fd), a2: uintptr(unsafe.Pointer(&msg))}
		libcCall(unsafe.Pointer(abi.FuncPCABI0(detfork_recvmsg_trampoline)), unsafe.Pointer(&c))
		if c.errno == _EINTR {
			continue
		}
		if c.errno != 0 || c.r != uintptr(unsafe.Sizeof(*cmd)) {
			return 0, false
		}
		break
	}
	KeepAlive(&msg)
	nfds := 0
	if msg.controllen >= 12 {
		clen := *(*uint32)(unsafe.Pointer(&control[0]))
		level := *(*int32)(unsafe.Pointer(&control[4]))
		typ := *(*int32)(unsafe.Pointer(&control[8]))
		if level == 0xffff && typ == 1 { // SOL_SOCKET, SCM_RIGHTS
			nfds = min(int(clen-12)/4, len(fds))
			for i := 0; i < nfds; i++ {
				fds[i] = *(*int32)(unsafe.Pointer(&control[12+4*i]))
			}
		}
	}
	return nfds, true
}
