// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux && (amd64 || arm64)

package sim

import (
	"cmp"
	"runtime"
	"slices"
	"syscall"
	"unsafe"
)

// rawTraps are the traps that the runtime's RawSyscall offers to handle:
// the socket calls package syscall makes raw. The runtime offers the
// first, socket, from goroutines of no node too (see noNodeSocket).
var rawTraps = [4]uintptr{syscall.SYS_SOCKET, syscall.SYS_GETSOCKNAME, syscall.SYS_GETPEERNAME, syscall.SYS_SOCKETPAIR}

// raw makes a real system call that a node's goroutine makes and the
// simulation leaves to the OS, such as a read of a real time zone file or
// an madvise of a mapping, without entering a system call as the syscall
// package would: the runtime would hand off the P of a goroutine that a
// loaded machine leaves in the call for 10ms, which changes the schedule.
// Each such call returns without waiting for anything else.
func raw(trap uintptr, a [6]uintptr) (r1, r2, errno uintptr, ok bool) {
	r1, r2, e := syscall.RawSyscall6(trap, a[0], a[1], a[2], a[3], a[4], a[5])
	return r1, r2, uintptr(e), true
}

// handle is the simulation's system call handler; see runtime/detsim.go.
// copied returns how many bytes a system call that returned r1 and errno
// copied between the caller's memory and a file or socket.
func copied(trap, r1, errno uintptr) int {
	switch trap {
	case syscall.SYS_READ, syscall.SYS_WRITE, syscall.SYS_PREAD64, syscall.SYS_PWRITE64,
		syscall.SYS_READV, syscall.SYS_WRITEV, syscall.SYS_RECVFROM, syscall.SYS_SENDTO,
		syscall.SYS_RECVMSG, syscall.SYS_SENDMSG, syscall.SYS_RECVMMSG, sysSendmmsg:
		if errno == 0 {
			return int(r1)
		}
	}
	return 0
}

func handle(node int32, trap uintptr, a [6]uintptr) (r1, r2, errno uintptr, ok bool) {
	w := world
	if w == nil {
		return
	}
	inc := w.incs[node]
	if node != 0 && inc == nil {
		// A goroutine of a crashed node, running on to release a
		// mutex (see the runtime's detsimSemacquire): its kernel is gone.
		return 0, 0, uintptr(syscall.EIO), true
	}
	switch trap {
	case syscall.SYS_READ, syscall.SYS_WRITE, syscall.SYS_PREAD64, syscall.SYS_PWRITE64,
		syscall.SYS_CLOSE, syscall.SYS_FSTAT, syscall.SYS_LSEEK, syscall.SYS_FSYNC,
		syscall.SYS_FDATASYNC, syscall.SYS_FTRUNCATE, syscall.SYS_GETDENTS64,
		syscall.SYS_FCNTL, syscall.SYS_FLOCK, syscall.SYS_FCHMOD, syscall.SYS_FCHOWN,
		syscall.SYS_FCHDIR, syscall.SYS_FSTATFS, syscall.SYS_FALLOCATE,
		syscall.SYS_SYNC_FILE_RANGE, syscall.SYS_READV, syscall.SYS_WRITEV,
		syscall.SYS_SHUTDOWN, syscall.SYS_GETSOCKOPT, syscall.SYS_SETSOCKOPT,
		syscall.SYS_BIND, syscall.SYS_LISTEN, syscall.SYS_ACCEPT4,
		syscall.SYS_CONNECT, syscall.SYS_GETSOCKNAME, syscall.SYS_GETPEERNAME,
		syscall.SYS_RECVFROM, syscall.SYS_SENDTO, syscall.SYS_RECVMSG,
		syscall.SYS_SENDMSG, syscall.SYS_RECVMMSG, sysSendmmsg,
		syscall.SYS_SENDFILE, syscall.SYS_SPLICE,
		sysCopyFileRange, syscall.SYS_DUP, syscall.SYS_DUP3:
		if trap == syscall.SYS_SENDFILE || trap == syscall.SYS_SPLICE || trap == sysCopyFileRange {
			// Either descriptor may be simulated; make the caller copy.
			if isSimFD(a[0]) || isSimFD(a[1]) || isSimFD(a[2]) {
				return 0, 0, uintptr(syscall.EINVAL), true
			}
			return
		}
		if !isSimFD(a[0]) {
			if inc != nil && trap != syscall.SYS_FCNTL && trap != syscall.SYS_FLOCK {
				return raw(trap, a) // a real file the node reads, or its standard output
			}
			return
		}
		d := w.fds[int(a[0])]
		if d == nil {
			return 0, 0, eBADF, true
		}
		if d.sock != nil {
			r1, errno = w.sockCall(d, trap, a)
		} else {
			r1, errno = w.fileCall(d, trap, a)
		}
		return r1, 0, errno, true

	case syscall.SYS_OPENAT, sysNewfstatat, syscall.SYS_MKDIRAT,
		syscall.SYS_UNLINKAT, syscall.SYS_RENAMEAT, sysRenameat2,
		syscall.SYS_READLINKAT, syscall.SYS_FACCESSAT, sysFaccessat2,
		syscall.SYS_FCHMODAT, syscall.SYS_FCHOWNAT, syscall.SYS_UTIMENSAT,
		syscall.SYS_SYMLINKAT, syscall.SYS_LINKAT, syscall.SYS_MKNODAT,
		sysStatx, sysOpenat2, syscall.SYS_INOTIFY_ADD_WATCH:
		if inc == nil && !isSimFD(a[0]) {
			return
		}
		if realPath(trap, a) {
			if inc != nil {
				return raw(trap, a)
			}
			return
		}
		r1, errno = w.pathCall(inc, trap, a)
		return r1, 0, errno, true

	case syscall.SYS_GETCWD, syscall.SYS_CHDIR, syscall.SYS_TRUNCATE,
		syscall.SYS_STATFS, syscall.SYS_CHROOT:
		if inc == nil {
			return
		}
		r1, errno = w.pathCall(inc, trap, a)
		return r1, 0, errno, true

	case syscall.SYS_MMAP:
		if !isSimFD(a[4]) {
			return
		}
		d := w.fds[int(a[4])]
		if d == nil || d.file == nil {
			return 0, 0, eBADF, true
		}
		r1, errno = w.mmap(d.file, d.inc, a)
		return r1, 0, errno, true

	case syscall.SYS_MREMAP:
		if r1, errno, ok = w.mremap(a); ok {
			// The kernel leaves the register that held the new size
			// (rdx, the second result on amd64) as it was, which
			// programs rely on, such as ristretto's z.MmapFile.
			r2 = a[1]
			if runtime.GOARCH == "amd64" {
				r2 = a[2]
			}
			return r1, r2, errno, true
		}
		return

	case syscall.SYS_MUNMAP:
		// The kernel unmaps the memory; forget the mapping first.
		if w.munmap(a[0], a[1]) || inc != nil {
			return raw(trap, a)
		}
		return

	case syscall.SYS_MSYNC:
		const msSync = 4 // MS_SYNC
		if errno, ok = w.msync(a[0], a[1], a[2]&msSync != 0); ok {
			return 0, 0, errno, true
		}
		if inc != nil {
			return raw(trap, a)
		}

	case syscall.SYS_MADVISE, syscall.SYS_MPROTECT:
		if inc != nil {
			return raw(trap, a)
		}

	case syscall.SYS_SOCKET, syscall.SYS_SOCKETPAIR:
		if inc == nil {
			if trap == syscall.SYS_SOCKET && w.noNodeSocket(a[0]) {
				return 0, 0, uintptr(syscall.EPERM), true
			}
			return
		}
		r1, errno = w.socketCall(inc, trap, a)
		return r1, 0, errno, true

	case syscall.SYS_GETPID:
		if inc == nil {
			return
		}
		return uintptr(inc.pid()), 0, 0, true

	case syscall.SYS_INOTIFY_INIT1, sysInotifyInit:
		if inc == nil {
			return
		}
		return ^uintptr(0), 0, w.noWatch("inotify_init1"), true // -1, which callers check
	}
	return
}

// realPrefixes are the read-only system files that nodes read from the
// real machine, where their contents are fixed: time zones and
// certificates.
var realPrefixes = []string{
	"/usr/share/zoneinfo/", "/etc/localtime", "/etc/ssl/", "/etc/pki/",
	"/usr/lib/ssl/", "/usr/share/ca-certificates/", "/etc/ca-certificates",
	"/usr/local/share/certs/", "/etc/mime.types",
}

// mapFixedNoReplace is MAP_FIXED_NOREPLACE, since Linux 4.17; older
// kernels take the address as a hint.
const mapFixedNoReplace = 0x100000

func (w *World) mmap(f *openFile, inc *incarnation, a [6]uintptr) (uintptr, uintptr) {
	n := f.ino
	if n.dir || a[3]&syscall.MAP_FIXED != 0 || int64(a[5]) < 0 || a[5]%4096 != 0 {
		return 0, uintptr(syscall.EINVAL)
	}
	shared := a[2]&syscall.PROT_WRITE != 0 && a[3]&syscall.MAP_SHARED != 0
	if shared && f.flags&syscall.O_ACCMODE != syscall.O_RDWR {
		return 0, uintptr(syscall.EACCES)
	}
	m := mapping{ino: n, disk: f.disk, inc: inc, off: int64(a[5]), len: a[1], prot: a[2], shared: shared, aliased: a[3]&syscall.MAP_SHARED != 0}
	if !m.aliased {
		// The copy starts as the page cache is, with the stores to the
		// file's shared mappings.
		w.writeBackFile(n)
		addr, e := mapMemory(a[1], syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS, ^uintptr(0), 0)
		if e != 0 {
			return 0, e
		}
		m.addr = addr
		w.addMap(m)
		w.fillMapping(m)
		return addr, 0
	}
	if e := w.growShm(n, m.off+int64(m.len)); e != 0 {
		return 0, e
	}
	n.shm.maps++
	addr, e := mapMemory(m.len, m.prot, syscall.MAP_SHARED, n.shm.fd, uintptr(m.off))
	if e != 0 {
		w.releaseShm(n)
		return 0, e
	}
	m.addr = addr
	w.addMap(m)
	return addr, 0
}

// A shmem is the memory that the shared mappings of a file map: a memfd
// holding the file's bytes, as the page cache holds them, so that a store
// through one mapping shows through the others at once, as on Linux. It
// covers the file's mappings, from offset 0 to size, and holds the file's
// bytes there, zeros past its end, and the program's stores that the
// simulation has not written back yet. The simulation writes no page of
// only zeros into a hole, and each write-back compares only the pages
// that are not holes (dataPages), so that a large, mostly empty mapping
// costs what it holds. It reads and writes the memory through view, its
// own mapping of all of it. The memfd goes with the file's last shared
// mapping, which a later one fills again from the file.
type shmem struct {
	fd   uintptr
	view uintptr
	size int64  // a multiple of the page size
	maps int    // the mappings of it
	fork uint64 // the fork that last copied it (see forked)
}

const (
	mfdCloexec    = 1 // MFD_CLOEXEC
	seekData      = 3 // SEEK_DATA
	seekHole      = 4 // SEEK_HOLE
	fallocPunch   = 3 // FALLOC_FL_PUNCH_HOLE | FALLOC_FL_KEEP_SIZE
	mremapMayMove = 1 // MREMAP_MAYMOVE
)

var memfdName = [...]byte{'s', 'i', 'm', 0}

// growShm makes n's shmem cover its bytes up to end, creating it if n has
// none, and fills the new part from the file.
func (w *World) growShm(n *inode, end int64) uintptr {
	size := (end + pageSize - 1) &^ (pageSize - 1)
	s := n.shm
	if s == nil {
		fd, _, e := syscall.RawSyscall(sysMemfdCreate, uintptr(unsafe.Pointer(&memfdName[0])), mfdCloexec, 0)
		if e != 0 {
			return uintptr(e)
		}
		s = &shmem{fd: fd}
		n.shm = s
	} else if size <= s.size {
		return 0
	}
	_, _, e := syscall.RawSyscall(syscall.SYS_FTRUNCATE, s.fd, uintptr(size), 0)
	view := s.view
	if e == 0 && view == 0 {
		view, _, e = syscall.RawSyscall6(syscall.SYS_MMAP, 0, uintptr(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED, s.fd, 0)
	} else if e == 0 {
		view, _, e = syscall.RawSyscall6(syscall.SYS_MREMAP, view, uintptr(s.size), uintptr(size), mremapMayMove, 0, 0)
	}
	if e != 0 {
		if s.maps == 0 {
			w.freeShm(n)
		}
		return uintptr(e)
	}
	old := s.size
	s.view, s.size = view, size
	// The new pages are holes: write the file's pages that are not all
	// zeros, in runs.
	lo, fill := int64(-1), min(size, int64(len(n.data)))
	for off := old; lo >= 0 || off < fill; off += pageSize {
		if off < fill && !allZero(n.data[off:min(off+pageSize, fill)]) {
			if lo < 0 {
				lo = off
			}
			continue
		}
		if lo >= 0 && !pwriteAll(s.fd, n.data[lo:min(off, fill)], lo) {
			if s.maps == 0 {
				w.freeShm(n)
			}
			return uintptr(syscall.ENOMEM)
		}
		lo = -1
	}
	return 0
}

// pwriteAll writes b to fd at off.
func pwriteAll(fd uintptr, b []byte, off int64) bool {
	for len(b) > 0 {
		r, _, e := syscall.RawSyscall6(syscall.SYS_PWRITE64, fd, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(off), 0, 0)
		if e == syscall.EINTR {
			continue
		}
		if e != 0 || r == 0 {
			return false
		}
		b, off = b[r:], off+int64(r)
	}
	return true
}

// releaseShm drops a mapping's hold on n's shmem.
func (w *World) releaseShm(n *inode) {
	if n.shm.maps--; n.shm.maps == 0 {
		w.freeShm(n)
	}
}

func (w *World) freeShm(n *inode) {
	s := n.shm
	if s.view != 0 {
		syscall.RawSyscall(syscall.SYS_MUNMAP, s.view, uintptr(s.size), 0)
	}
	syscall.RawSyscall(syscall.SYS_CLOSE, s.fd, 0, 0)
	n.shm = nil
}

// dataPages reports, for each page that [lo, hi) of s touches, whether
// it may hold more than zeros: it is not a hole.
func (s *shmem) dataPages(lo, hi int64) []bool {
	first := lo / pageSize
	pages := make([]bool, (hi+pageSize-1)/pageSize-first)
	for off := first * pageSize; off < hi; {
		d, _, e := syscall.RawSyscall(syscall.SYS_LSEEK, s.fd, uintptr(off), seekData)
		if e == syscall.ENXIO {
			break // only a hole from off on
		}
		h := uintptr(s.size)
		if e != 0 {
			d, h = uintptr(off), uintptr(hi) // cannot tell
		} else if r, _, e := syscall.RawSyscall(syscall.SYS_LSEEK, s.fd, d, seekHole); e == 0 {
			h = r
		}
		for p := int64(d) / pageSize; p*pageSize < min(int64(h), hi); p++ {
			pages[p-first] = true
		}
		off = int64(h)
	}
	return pages
}

// syncShm updates n's shmem after a change to the file's bytes [lo, hi).
// Pages that become all zeros become holes again.
func (w *World) syncShm(n *inode, lo, hi int64) {
	s := n.shm
	lo, hi = max(lo, 0), min(hi, s.size)
	if lo >= hi {
		return
	}
	data := s.dataPages(lo, hi)
	view := mem(s.view, uintptr(s.size))
	hole := int64(-1) // the start of a run of pages to punch
	for off := lo; off <= hi; {
		pe := min((off/pageSize+1)*pageSize, hi)
		var src []byte
		if off < int64(len(n.data)) {
			src = n.data[off:min(pe, int64(len(n.data)))]
		}
		empty := off < hi && allZero(src)
		if off < hi && empty && data[off/pageSize-lo/pageSize] && off%pageSize == 0 && pe-off == pageSize {
			if hole < 0 {
				hole = off
			}
			off = pe
			continue
		}
		if hole >= 0 {
			syscall.RawSyscall6(syscall.SYS_FALLOCATE, s.fd, fallocPunch, uintptr(hole), uintptr(off-hole), 0, 0)
			hole = -1
		}
		if off == hi {
			break
		}
		switch {
		case !data[off/pageSize-lo/pageSize]:
			// A hole holds zeros already.
			if !empty {
				copy(view[off:], src)
			}
		default:
			k := copy(view[off:pe], src)
			clear(view[off+int64(k) : pe])
		}
		off = pe
	}
}

// writeBackShm copies the program's stores to n's shared mappings into
// the file (see writeBackMem). Holes still hold zeros, as the file does
// there (see syncShm).
func (w *World) writeBackShm(n *inode) {
	s := n.shm
	end := min(s.size, n.size)
	if end <= 0 {
		return
	}
	w.writeBackMem(n, mem(s.view, uintptr(end)), 0, s.dataPages(0, end))
}

// forks counts the forks of the process (see forked).
var forks uint64

// forked gives the child of a fork its own copy of each shmem, mapped
// where the parent's was, as its anonymous memory is its own; otherwise
// the stores of a child, such as a branch of the timeline (Branch), would
// show in the parent's mappings. The runtime calls it in the child before
// any goroutine runs. It allocates nothing, and the descriptors and
// addresses stay the same, so that the child's schedule is that of a
// branch run in place (GODETBRANCH).
func forked() {
	w := world
	if w == nil {
		return
	}
	forks++
	for _, m := range w.maps {
		if !m.aliased || m.ino.shm.fork == forks {
			continue
		}
		s := m.ino.shm
		s.fork = forks
		fd, _, e := syscall.RawSyscall(sysMemfdCreate, uintptr(unsafe.Pointer(&memfdName[0])), mfdCloexec, 0)
		if e == 0 {
			_, _, e = syscall.RawSyscall(syscall.SYS_FTRUNCATE, fd, uintptr(s.size), 0)
		}
		for off := uintptr(0); e == 0 && off < uintptr(s.size); {
			d, _, e2 := syscall.RawSyscall(syscall.SYS_LSEEK, s.fd, off, seekData)
			if e2 == syscall.ENXIO {
				break
			}
			h, _, e3 := syscall.RawSyscall(syscall.SYS_LSEEK, s.fd, d, seekHole)
			if e3 != 0 {
				h = uintptr(s.size)
			}
			if e = e2; e == 0 && !pwriteAll(fd, mem(s.view+d, h-d), int64(d)) {
				e = syscall.EIO
			}
			off = h
		}
		if e == 0 {
			_, _, e = syscall.RawSyscall6(syscall.SYS_MMAP, s.view, uintptr(s.size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_FIXED, fd, 0)
		}
		if e == 0 {
			_, _, e = syscall.RawSyscall(syscall.SYS_DUP3, fd, s.fd, syscall.O_CLOEXEC)
		}
		if e != 0 {
			forkFailed()
		}
		syscall.RawSyscall(syscall.SYS_CLOSE, fd, 0, 0)
	}
	for _, m := range w.maps {
		if m.aliased {
			if _, _, e := syscall.RawSyscall6(syscall.SYS_MMAP, m.addr, m.len, m.prot, syscall.MAP_SHARED|syscall.MAP_FIXED, m.ino.shm.fd, uintptr(m.off)); e != 0 {
				forkFailed()
			}
		}
	}
}

func forkFailed() {
	msg := []byte("sim: cannot copy the shared mappings in a forked child\n")
	syscall.RawSyscall(syscall.SYS_WRITE, 2, uintptr(unsafe.Pointer(&msg[0])), uintptr(len(msg)))
	syscall.RawSyscall(syscall.SYS_EXIT_GROUP, 99, 0, 0)
}

// mapMemory maps length bytes for a mapping, at the next address after
// the last mapping, not where Linux would put it: anonymous memory for a
// private mapping, writable so that writes to the file can update it, or
// a shmem's memfd for a shared one.
func mapMemory(length, prot, flags, fd, off uintptr) (uintptr, uintptr) {
	addr, _, e := syscall.RawSyscall6(syscall.SYS_MMAP, nextMapAddr(length), length, prot, flags|mapFixedNoReplace, fd, off)
	if e != 0 {
		// Taken, or beyond the address space: let the kernel pick.
		addr, _, e = syscall.RawSyscall6(syscall.SYS_MMAP, 0, length, prot, flags, fd, off)
	}
	if e == 0 && length >= 2<<20 && flags&syscall.MAP_ANONYMOUS != 0 {
		// The copy of the file faults every page in at once: huge pages
		// take a fault per 2 MB instead of per 4 KB where the system
		// grants them only on request.
		syscall.RawSyscall(syscall.SYS_MADVISE, addr, length, syscall.MADV_HUGEPAGE)
	}
	return addr, uintptr(e)
}

// mremap resizes a simulated mapping, and reports whether a[0] is one. A
// mapping that grows moves to new memory (with MREMAP_MAYMOVE; otherwise
// it fails with ENOMEM), at an address that mapMemory chooses, after its
// writes are written back.
func (w *World) mremap(a [6]uintptr) (r1, errno uintptr, ok bool) {
	const mayMove, fixed = 1, 2
	i, found := slices.BinarySearchFunc(w.maps, a[0], func(m mapping, addr uintptr) int { return cmp.Compare(m.addr, addr) })
	if !found {
		return 0, 0, false
	}
	m := w.maps[i]
	size := a[2]
	round := func(n uintptr) uintptr { return (n + pageSize - 1) &^ (pageSize - 1) }
	switch {
	case a[3]&fixed != 0 || size == 0 || round(a[1]) != round(m.len):
		return 0, uintptr(syscall.EINVAL), true
	case round(size) <= round(m.len):
		w.writeBack(m)
		if tail := round(m.len) - round(size); tail > 0 {
			syscall.RawSyscall(syscall.SYS_MUNMAP, m.addr+round(size), tail, 0)
		}
		w.maps[i].len = size
		return m.addr, 0, true
	case a[3]&mayMove == 0:
		return 0, uintptr(syscall.ENOMEM), true
	}
	w.writeBackFile(m.ino)
	var addr, e uintptr
	if m.aliased {
		if e = w.growShm(m.ino, m.off+int64(size)); e != 0 {
			return 0, e, true
		}
		addr, e = mapMemory(size, m.prot, syscall.MAP_SHARED, m.ino.shm.fd, uintptr(m.off))
	} else {
		addr, e = mapMemory(size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS, ^uintptr(0), 0)
	}
	if e != 0 {
		return 0, e, true
	}
	syscall.RawSyscall(syscall.SYS_MUNMAP, m.addr, m.len, 0)
	m.addr, m.len = addr, size
	w.maps = slices.Delete(w.maps, i, i+1)
	i, _ = slices.BinarySearchFunc(w.maps, addr, func(o mapping, addr uintptr) int { return cmp.Compare(o.addr, addr) })
	w.maps = slices.Insert(w.maps, i, m)
	if !m.aliased {
		w.fillMapping(m)
	}
	return addr, 0, true
}
