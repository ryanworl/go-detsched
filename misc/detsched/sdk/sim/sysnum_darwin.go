// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin && !ios && arm64

package sim

// The system calls of the simulated kernel, which speaks Linux's system
// call interface: on darwin, numbers of the simulation's own, which the
// frontend (kernel_darwin.go) translates libc calls into.
const (
	sysAccept4 = 1 + iota
	sysBind
	sysChdir
	sysChroot
	sysClose
	sysConnect
	sysDup
	sysDup3
	sysFaccessat
	sysFaccessat2
	sysFchdir
	sysFchmod
	sysFchmodat
	sysFchown
	sysFchownat
	sysFallocate // darwin has none
	sysFcntl
	sysFdatasync
	sysFlock
	sysFstat
	sysFstatfs
	sysFsync
	sysFtruncate
	sysGetcwd
	sysGetdents64
	sysGetpeername
	sysGetsockname
	sysGetsockopt
	sysInotifyAddWatch
	sysLinkat
	sysListen
	sysLseek
	sysMkdirat
	sysMknodat
	sysNewfstatat
	sysOpenat
	sysOpenat2
	sysPread64
	sysPwrite64
	sysRead
	sysReadlinkat
	sysReadv
	sysRecvfrom
	sysRecvmmsg
	sysRecvmsg
	sysRenameat
	sysRenameat2
	sysSendmmsg
	sysSendmsg
	sysSendto
	sysSetsockopt
	sysShutdown
	sysSocket
	sysSocketpair
	sysStatfs
	sysStatx
	sysSymlinkat
	sysSyncFileRange
	sysTruncate
	sysUnlinkat
	sysUtimensat
	sysWrite
	sysWritev
)
