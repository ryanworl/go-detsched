// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && (amd64 || arm64)

package runtime

import (
	"internal/goarch"
	"internal/runtime/syscall/linux"
	"unsafe"
)

// The system calls of fork mode on Linux; see detfork.go.

const (
	detSysClone   = 56*goarch.IsAmd64 + 220*goarch.IsArm64
	detSysWait4   = 61*goarch.IsAmd64 + 260*goarch.IsArm64
	detSysDup3    = 292*goarch.IsAmd64 + 24*goarch.IsArm64
	detSysRecvmsg = 47*goarch.IsAmd64 + 212*goarch.IsArm64

	detSIGCHLD = 17
)

// detForkFork forks the process with a raw clone. It returns the child's
// PID in the parent, and 0 in the child.
func detForkFork() (r, errno uintptr) {
	r, _, errno = linux.Syscall6(detSysClone, detSIGCHLD, 0, 0, 0, 0, 0)
	return r, errno
}

// detForkSelf records the thread ID of the child's only thread.
func detForkSelf() {
	getg().m.procid = uint64(gettid())
}

// detForkResetM clears the semaphore of mp, an M in a child, unless mp is
// m0, which forked.
func detForkResetM(mp *m) {
	if mp != &m0 {
		mp.waitsema = 0
	}
}

// detForkPollReinit gives a child a new epoll instance and event fd.
func detForkPollReinit() {
	closefd(epfd)
	closefd(int32(netpollEventFd))
	netpollinit()
}

func detForkDup2(oldfd, newfd int32) {
	linux.Syscall6(detSysDup3, uintptr(oldfd), uintptr(newfd), 0, 0, 0, 0)
}

// detForkWait4 waits for the child pid (-1: any child) with the given
// options and returns its PID, or 0 with WNOHANG if none exited.
func detForkWait4(pid int64, status *int32, options uintptr) (r, errno uintptr) {
	r, _, errno = linux.Syscall6(detSysWait4, uintptr(pid), uintptr(unsafe.Pointer(status)), options, 0, 0, 0)
	return r, errno
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
		_          uint32
		iov        *iovec
		iovlen     uint64
		control    *byte
		controllen uint64
		flags      int32
		_          int32
	}
	var control [16 + 4*6]byte // cmsghdr (len uint64, level, type int32), then the fds, padded to 8 bytes
	iov := iovec{(*byte)(unsafe.Pointer(cmd)), uint64(unsafe.Sizeof(*cmd))}
	msg := msghdr{iov: &iov, iovlen: 1, control: &control[0], controllen: uint64(len(control))}
	for {
		n, _, e := linux.Syscall6(detSysRecvmsg, uintptr(detFork.fd), uintptr(unsafe.Pointer(&msg)), 0, 0, 0, 0)
		if e == _EINTR {
			continue
		}
		if e != 0 || n != uintptr(unsafe.Sizeof(*cmd)) {
			return 0, false
		}
		break
	}
	nfds := 0
	if msg.controllen >= 16 {
		clen := *(*uint64)(unsafe.Pointer(&control[0]))
		level := *(*int32)(unsafe.Pointer(&control[8]))
		typ := *(*int32)(unsafe.Pointer(&control[12]))
		if level == 1 && typ == 1 { // SOL_SOCKET, SCM_RIGHTS
			nfds = min(int(clen-16)/4, len(fds))
			for i := 0; i < nfds; i++ {
				fds[i] = *(*int32)(unsafe.Pointer(&control[16+4*i]))
			}
		}
	}
	return nfds, true
}
