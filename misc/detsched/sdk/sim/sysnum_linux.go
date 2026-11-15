// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && (amd64 || arm64)

package sim

import "syscall"

// The system calls of the simulated kernel, which speaks Linux's system
// call interface: on Linux, their numbers.
const (
	sysAccept4         = syscall.SYS_ACCEPT4
	sysBind            = syscall.SYS_BIND
	sysChdir           = syscall.SYS_CHDIR
	sysChroot          = syscall.SYS_CHROOT
	sysClose           = syscall.SYS_CLOSE
	sysConnect         = syscall.SYS_CONNECT
	sysDup             = syscall.SYS_DUP
	sysDup3            = syscall.SYS_DUP3
	sysFaccessat       = syscall.SYS_FACCESSAT
	sysFchdir          = syscall.SYS_FCHDIR
	sysFchmod          = syscall.SYS_FCHMOD
	sysFchmodat        = syscall.SYS_FCHMODAT
	sysFchown          = syscall.SYS_FCHOWN
	sysFchownat        = syscall.SYS_FCHOWNAT
	sysFcntl           = syscall.SYS_FCNTL
	sysFallocate       = syscall.SYS_FALLOCATE
	sysFdatasync       = syscall.SYS_FDATASYNC
	sysFlock           = syscall.SYS_FLOCK
	sysFstat           = syscall.SYS_FSTAT
	sysFstatfs         = syscall.SYS_FSTATFS
	sysFsync           = syscall.SYS_FSYNC
	sysFtruncate       = syscall.SYS_FTRUNCATE
	sysGetcwd          = syscall.SYS_GETCWD
	sysGetdents64      = syscall.SYS_GETDENTS64
	sysGetpeername     = syscall.SYS_GETPEERNAME
	sysGetsockname     = syscall.SYS_GETSOCKNAME
	sysGetsockopt      = syscall.SYS_GETSOCKOPT
	sysInotifyAddWatch = syscall.SYS_INOTIFY_ADD_WATCH
	sysLinkat          = syscall.SYS_LINKAT
	sysListen          = syscall.SYS_LISTEN
	sysLseek           = syscall.SYS_LSEEK
	sysMkdirat         = syscall.SYS_MKDIRAT
	sysMknodat         = syscall.SYS_MKNODAT
	sysOpenat          = syscall.SYS_OPENAT
	sysPread64         = syscall.SYS_PREAD64
	sysPwrite64        = syscall.SYS_PWRITE64
	sysRead            = syscall.SYS_READ
	sysReadlinkat      = syscall.SYS_READLINKAT
	sysReadv           = syscall.SYS_READV
	sysRecvfrom        = syscall.SYS_RECVFROM
	sysRecvmmsg        = syscall.SYS_RECVMMSG
	sysRecvmsg         = syscall.SYS_RECVMSG
	sysRenameat        = syscall.SYS_RENAMEAT

	sysSendmsg       = syscall.SYS_SENDMSG
	sysSendto        = syscall.SYS_SENDTO
	sysSetsockopt    = syscall.SYS_SETSOCKOPT
	sysShutdown      = syscall.SYS_SHUTDOWN
	sysSocketpair    = syscall.SYS_SOCKETPAIR
	sysStatfs        = syscall.SYS_STATFS
	sysSymlinkat     = syscall.SYS_SYMLINKAT
	sysSyncFileRange = syscall.SYS_SYNC_FILE_RANGE
	sysTruncate      = syscall.SYS_TRUNCATE
	sysUnlinkat      = syscall.SYS_UNLINKAT
	sysUtimensat     = syscall.SYS_UTIMENSAT
	sysWrite         = syscall.SYS_WRITE
	sysWritev        = syscall.SYS_WRITEV
)
