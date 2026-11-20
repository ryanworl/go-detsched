// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin && !ios && arm64

package sim

import (
	"syscall"
	"unsafe"
)

// The darwin frontend of the simulated disk: flags, stat, and the calls
// darwin has and Linux doesn't. os.File.ReadDir duplicates the descriptor
// with fcntl(F_DUPFD_CLOEXEC), opens the duplicate with fdopendir, reads
// it with readdir_r, and closes it with closedir. A simulated directory's
// DIR* is the duplicate descriptor itself, which is never a valid pointer
// on darwin/arm64, where nothing is mapped below 4GB.

const (
	_AT_FDCWD            = -2
	_AT_REMOVEDIR        = 0x80
	_AT_SYMLINK_NOFOLLOW = 0x20
	_AT_SYMLINK_FOLLOW   = 0x40
	_AT_EMPTY_PATH       = 0 // darwin has none

	_UTIME_OMIT = -2

	// darwin has no open file description locks; no fcntl command
	// matches these.
	_F_OFD_GETLK  = 1<<20 + 36
	_F_OFD_SETLK  = 1<<20 + 37
	_F_OFD_SETLKW = 1<<20 + 38

	_F_PREALLOCATE = syscall.F_PREALLOCATE
)

// preallocate is fcntl(F_PREALLOCATE) with the fstore_t at p: it gives
// the file blocks without changing its size, from its allocated end
// (F_PEOFPOSMODE) or from the offset, as fallocate's
// FALLOC_FL_KEEP_SIZE does.
func (w *World) preallocate(f *openFile, p unsafe.Pointer) uintptr {
	fs := (*syscall.Fstore_t)(p)
	off := fs.Offset
	switch fs.Posmode {
	case syscall.F_PEOFPOSMODE:
		off += f.ino.alloc
	case syscall.F_VOLPOSMODE:
	default:
		return uintptr(syscall.EINVAL)
	}
	fs.Bytesalloc = 0
	if e := w.fallocate(f, false, off, fs.Length); e != 0 {
		return e
	}
	fs.Bytesalloc = fs.Length
	return 0
}

// fillStatfs fills the statfs result at p for dk: a file system of 64GiB
// that is empty unless the disk is full, as free space is not simulated.
func fillStatfs(dk *disk, p unsafe.Pointer) {
	st := (*syscall.Statfs_t)(p)
	*st = syscall.Statfs_t{Bsize: 4096, Iosize: 4096, Blocks: 1 << 24, Files: 1 << 22}
	if !dk.full && now() >= dk.fullUntil {
		st.Bfree, st.Bavail, st.Ffree = st.Blocks, st.Blocks, st.Files
	}
}

func fillStat(dk *disk, n *inode, st *syscall.Stat_t) {
	*st = syscall.Stat_t{}
	st.Dev = int32(dk.dev)
	st.Ino = n.ino
	st.Nlink = 1
	st.Mode = uint16(n.perm)
	if n.dir {
		st.Mode |= syscall.S_IFDIR
		st.Nlink = 2
	} else {
		st.Mode |= syscall.S_IFREG
		st.Size = n.size
		st.Blocks = (st.Size + 511) / 512
	}
	st.Blksize = 4096
	st.Mtimespec = syscall.NsecToTimespec(n.mtime)
	st.Ctimespec = st.Mtimespec
	st.Atimespec = st.Mtimespec
	st.Birthtimespec = st.Mtimespec
}

// dup returns a new descriptor for d's open file, sharing its offset.
func (w *World) dup(d *fdesc) (uintptr, uintptr) {
	if d.file == nil {
		return 0, uintptr(syscall.EMFILE)
	}
	return uintptr(w.newFD(&fdesc{inc: d.inc, file: d.file})), 0
}

// fdopendir returns the DIR* of the directory descriptor fd: fd itself.
func (w *World) fdopendir(d *fdesc, fd uintptr) (uintptr, uintptr) {
	if d.file == nil || !d.file.ino.dir {
		return 0, uintptr(syscall.ENOTDIR)
	}
	return fd, 0
}

// readdir is readdir_r(dir, entry, result) on the directory descriptor
// d. Like getdents on Linux, it lists the directory when first called
// and skips entries removed since.
func (w *World) readdir(d *fdesc, entry, result uintptr) uintptr {
	if d.file == nil || !d.file.ino.dir {
		return uintptr(syscall.EBADF)
	}
	f := d.file
	n := f.ino
	if !f.listed {
		f.names = append([]string{".", ".."}, sortedNames(n.ents)...)
		f.listed = true
		f.off = 0
	}
	for f.off < int64(len(f.names)) {
		name := f.names[f.off]
		f.off++
		c := child(n, name)
		if c == nil {
			continue // removed since the listing
		}
		de := (*syscall.Dirent)(ptr(entry))
		*de = syscall.Dirent{}
		de.Ino = c.ino
		de.Seekoff = uint64(f.off)
		de.Reclen = uint16(unsafe.Sizeof(*de))
		de.Namlen = uint16(len(name))
		de.Type = syscall.DT_REG
		if c.dir {
			de.Type = syscall.DT_DIR
		}
		copy(unsafe.Slice((*byte)(unsafe.Pointer(&de.Name[0])), len(de.Name)-1), name)
		*(*uintptr)(ptr(result)) = entry
		return 0
	}
	*(*uintptr)(ptr(result)) = 0
	return 0
}
