// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && (amd64 || arm64)

package sim

import (
	"syscall"
	"unsafe"
)

// The Linux frontend of the simulated disk: flags and stat.

const (
	_AT_FDCWD            = -100
	_AT_REMOVEDIR        = 0x200
	_AT_SYMLINK_NOFOLLOW = 0x100
	_AT_SYMLINK_FOLLOW   = 0x400
	_AT_EMPTY_PATH       = 0x1000

	_UTIME_OMIT = (1 << 30) - 2

	// fcntl's open file description locks.
	_F_OFD_GETLK  = 36
	_F_OFD_SETLK  = 37
	_F_OFD_SETLKW = 38

	// Linux has no fcntl(F_PREALLOCATE); no fcntl command matches it.
	_F_PREALLOCATE = 1<<20 + 42
)

func (w *World) preallocate(*openFile, unsafe.Pointer) uintptr { panic("unreachable") }

// fillStatfs fills the statfs result at p for dk: an ext4 file system of
// 64GiB that is empty unless the disk is full, as free space is not
// simulated.
func fillStatfs(dk *disk, p unsafe.Pointer) {
	st := (*syscall.Statfs_t)(p)
	*st = syscall.Statfs_t{Type: 0xef53, Bsize: 4096, Frsize: 4096, Namelen: 255, Blocks: 1 << 24, Files: 1 << 22}
	if !dk.full && now() >= dk.fullUntil {
		st.Bfree, st.Bavail, st.Ffree = st.Blocks, st.Blocks, st.Files
	}
}

func fillStat(dk *disk, n *inode, st *syscall.Stat_t) {
	*st = syscall.Stat_t{}
	st.Dev = dk.dev
	st.Ino = n.ino
	st.Nlink = 1
	st.Mode = n.perm
	if n.dir {
		st.Mode |= syscall.S_IFDIR
		st.Nlink = 2
	} else {
		st.Mode |= syscall.S_IFREG
		st.Size = n.size
		st.Blocks = (st.Size + 511) / 512
	}
	st.Blksize = 4096
	st.Mtim = syscall.NsecToTimespec(n.mtime)
	st.Ctim = st.Mtim
	st.Atim = st.Mtim
}
