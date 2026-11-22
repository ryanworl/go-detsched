// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin && !ios && arm64

package sim

import (
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

// The darwin frontend of the simulated kernel.
//
// On darwin, packages syscall and golang.org/x/sys/unix call libc, and
// the runtime offers each call to handle with the address of the libc
// trampoline it calls in place of a system call number (see
// runtime/detsim_darwin.go). Each package has its own trampolines, named
// libc_<function>_trampoline (and the runtime's fcntl_trampoline, for
// internal/syscall/unix.Fcntl); libcOf looks up the function of a
// trampoline by its name, once per address. handle translates the calls
// the kernel handles into Linux's form: open(path, ...) becomes
// openat(AT_FDCWD, path, ...), stat becomes newfstatat, accept becomes
// accept4, fcntl(F_FULLFSYNC) becomes fsync, and fcntl(F_PREALLOCATE)
// allocates blocks as fallocate with FALLOC_FL_KEEP_SIZE does. The calls
// Linux doesn't have are darwin's own: fcntl(F_DUPFD_CLOEXEC), fdopendir,
// readdir_r, and closedir (fs_darwin.go).
//
// Name resolution goes through libc on darwin, even without cgo; on a
// node, getaddrinfo, getnameinfo, and libresolv's res_9_nsearch find no
// names, as on Linux, where the resolver finds no resolv.conf or hosts
// file on the node's disk and no DNS server on the network.

// rawTraps are the traps that the runtime's RawSyscall offers to handle,
// on Linux. On darwin, the runtime picks the raw socket calls by the
// trampolines' names itself.
var rawTraps [4]uintptr

// A libcFn is a libc function the frontend knows.
type libcFn uint8

const (
	fnUnknown libcFn = iota

	// Calls on a descriptor, its first argument.
	fnRead
	fnWrite
	fnPread
	fnPwrite
	fnReadv
	fnWritev
	fnClose
	fnFstat
	fnLseek
	fnFsync
	fnFtruncate
	fnFcntl
	fnFlock
	fnFchmod
	fnFchown
	fnFchdir
	fnFstatfs
	fnDup
	fnDup2
	fnShutdown
	fnGetsockopt
	fnSetsockopt
	fnBind
	fnListen
	fnAccept
	fnConnect
	fnGetsockname
	fnGetpeername
	fnRecvfrom
	fnSendto
	fnRecvmsg
	fnSendmsg
	fnSendfile
	fnFdopendir
	fnReaddirR // on a DIR*
	fnClosedir // on a DIR*

	// Calls that name a path, from the current directory or from a
	// directory descriptor (*at).
	fnOpen
	fnOpenat
	fnStat
	fnLstat
	fnFstatat
	fnMkdir
	fnMkdirat
	fnUnlink
	fnRmdir
	fnUnlinkat
	fnRename
	fnRenameat
	fnRenameatxNp
	fnReadlink
	fnReadlinkat
	fnAccess
	fnFaccessat
	fnChmod
	fnFchmodat
	fnChown
	fnLchown
	fnFchownat
	fnUtimes
	fnUtimensat
	fnSymlink
	fnSymlinkat
	fnLink
	fnLinkat
	fnMknod
	fnMkfifo
	fnGetcwd
	fnChdir
	fnTruncate
	fnStatfs
	fnChroot

	fnMmap
	fnMunmap
	fnMadvise
	fnMprotect
	fnMsync

	fnSocket
	fnSocketpair
	fnGetpid
	fnKqueue

	fnGetaddrinfo
	fnGetnameinfo
	fnResNinit
	fnResNsearch
	fnResNclose
)

var libcFns = map[string]libcFn{
	"read": fnRead, "write": fnWrite, "pread": fnPread, "pwrite": fnPwrite,
	"readv": fnReadv, "writev": fnWritev, "close": fnClose,
	"fstat": fnFstat, "fstat64": fnFstat, "lseek": fnLseek, "fsync": fnFsync,
	"ftruncate": fnFtruncate, "fcntl": fnFcntl, "flock": fnFlock,
	"fchmod": fnFchmod, "fchown": fnFchown, "fchdir": fnFchdir,
	"fstatfs": fnFstatfs, "fstatfs64": fnFstatfs, "dup": fnDup, "dup2": fnDup2,
	"shutdown": fnShutdown, "getsockopt": fnGetsockopt, "setsockopt": fnSetsockopt,
	"bind": fnBind, "listen": fnListen, "accept": fnAccept, "connect": fnConnect,
	"getsockname": fnGetsockname, "getpeername": fnGetpeername,
	"recvfrom": fnRecvfrom, "sendto": fnSendto, "recvmsg": fnRecvmsg,
	"sendmsg": fnSendmsg, "sendfile": fnSendfile,
	"fdopendir": fnFdopendir, "readdir_r": fnReaddirR, "closedir": fnClosedir,

	"open": fnOpen, "openat": fnOpenat,
	"stat": fnStat, "stat64": fnStat, "lstat": fnLstat, "lstat64": fnLstat,
	"fstatat": fnFstatat, "fstatat64": fnFstatat,
	"mkdir": fnMkdir, "mkdirat": fnMkdirat,
	"unlink": fnUnlink, "rmdir": fnRmdir, "unlinkat": fnUnlinkat,
	"rename": fnRename, "renameat": fnRenameat, "renameatx_np": fnRenameatxNp,
	"readlink": fnReadlink, "readlinkat": fnReadlinkat,
	"access": fnAccess, "faccessat": fnFaccessat,
	"chmod": fnChmod, "fchmodat": fnFchmodat,
	"chown": fnChown, "lchown": fnLchown, "fchownat": fnFchownat,
	"utimes": fnUtimes, "utimensat": fnUtimensat,
	"symlink": fnSymlink, "symlinkat": fnSymlinkat, "link": fnLink, "linkat": fnLinkat,
	"mknod": fnMknod, "mkfifo": fnMkfifo,
	"getcwd": fnGetcwd, "chdir": fnChdir, "truncate": fnTruncate,
	"statfs": fnStatfs, "statfs64": fnStatfs, "chroot": fnChroot,

	"mmap": fnMmap, "munmap": fnMunmap, "madvise": fnMadvise,
	"mprotect": fnMprotect, "msync": fnMsync,

	"socket": fnSocket, "socketpair": fnSocketpair, "getpid": fnGetpid, "kqueue": fnKqueue,

	"getaddrinfo": fnGetaddrinfo, "getnameinfo": fnGetnameinfo,
	"res_9_ninit": fnResNinit, "res_9_nsearch": fnResNsearch, "res_9_nclose": fnResNclose,
}

// callName returns the name of the libc function that the trampoline at
// pc calls, for messages.
func callName(pc uintptr) string {
	if f := runtime.FuncForPC(pc); f != nil {
		name := f.Name()
		name = name[strings.LastIndex(name, ".")+1:]
		name = strings.TrimSuffix(name, "_trampoline")
		return strings.TrimPrefix(name, "libc_")
	}
	return "a libc function"
}

// libcByTrampoline caches libcOf.
var libcByTrampoline = map[uintptr]libcFn{}

// libcOf returns the libc function that the trampoline at pc calls.
func libcOf(pc uintptr) libcFn {
	if fn, ok := libcByTrampoline[pc]; ok {
		return fn
	}
	fn := fnUnknown
	if f := runtime.FuncForPC(pc); f != nil {
		// pkg.libc_read_trampoline, runtime.fcntl_trampoline, or
		// internal/syscall/unix.libresolv_res_9_nsearch_trampoline.
		name := f.Name()
		name = name[strings.LastIndex(name, ".")+1:]
		if name, ok := strings.CutSuffix(name, "_trampoline"); ok {
			name = strings.TrimPrefix(name, "libc_")
			name = strings.TrimPrefix(name, "libresolv_")
			fn = libcFns[name]
		}
	}
	libcByTrampoline[pc] = fn
	return fn
}

// rawSyscall and rawSyscall6 call a libc trampoline without entering a
// system call (golang.org/x/sys/unix uses them too). err is errno if r1
// is -1.
//
//go:linkname rawSyscall syscall.rawSyscall
func rawSyscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:linkname rawSyscall6 syscall.rawSyscall6
func rawSyscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

// raw makes the libc call fn through trampoline trap, which a node's
// goroutine makes and the simulation leaves to the OS, without entering a
// system call; see raw in kernel_linux.go. open and fcntl take their
// third argument as a variadic argument, which the trampoline passes on
// the stack only when called with three arguments.
func raw(fn libcFn, trap uintptr, a [6]uintptr) (r1, r2, errno uintptr, ok bool) {
	var e syscall.Errno
	if fn == fnOpen || fn == fnFcntl {
		r1, r2, e = rawSyscall(trap, a[0], a[1], a[2])
	} else {
		r1, r2, e = rawSyscall6(trap, a[0], a[1], a[2], a[3], a[4], a[5])
	}
	return r1, r2, uintptr(e), true
}

// result returns the result r or errno e of a call to fn as libc does:
// -1 and errno for most functions, NULL for those that return pointers,
// and the error number itself for readdir_r.
func result(fn libcFn, r, e uintptr) (r1, r2, errno uintptr, ok bool) {
	switch {
	case fn == fnReaddirR:
		return e, 0, 0, true
	case e == 0:
		return r, 0, 0, true
	case fn == fnFdopendir:
		return 0, 0, e, true
	}
	return ^uintptr(0), 0, e, true
}

// handle is the simulation's system call handler; see
// runtime/detsim_darwin.go.
// copied returns how many bytes a call that returned r1 and errno copied
// between the caller's memory and a file or socket.
func copied(trap, r1, errno uintptr) int {
	switch libcOf(trap) {
	case fnRead, fnWrite, fnPread, fnPwrite, fnReadv, fnWritev,
		fnRecvfrom, fnSendto, fnRecvmsg, fnSendmsg:
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
	fn := libcOf(trap)
	if fn == fnUnknown {
		return
	}
	w.call = trap
	inc := w.incs[node]
	if node != 0 && inc == nil {
		// A goroutine of a crashed node, running on to release a
		// mutex (see the runtime's detsimSemacquire): its kernel is gone.
		return 0, 0, uintptr(syscall.EIO), true
	}
	switch {
	case fn <= fnClosedir:
		if fn == fnSendfile {
			// Either descriptor may be simulated; make the caller copy,
			// as it does when sendfile sent nothing.
			if isSimFD(a[0]) || isSimFD(a[1]) {
				if a[3] != 0 {
					*(*int64)(ptr(a[3])) = 0
				}
				return result(fn, 0, uintptr(syscall.EINVAL))
			}
			return
		}
		if !isSimFD(a[0]) {
			// fdopendir returns a pointer, and raw would lose its errno.
			if inc != nil && fn != fnFcntl && fn != fnFlock && fn != fnFdopendir {
				return raw(fn, trap, a) // a real file the node reads, or its standard output
			}
			return
		}
		d, e := w.fd(inc, a[0])
		if e != 0 {
			return result(fn, 0, e)
		}
		var r uintptr
		switch {
		case fn == fnDup || fn == fnFcntl && (a[1] == syscall.F_DUPFD_CLOEXEC || a[1] == syscall.F_DUPFD):
			r, e = w.dup(d)
		case fn == fnFdopendir:
			r, e = w.fdopendir(d, a[0])
		case fn == fnReaddirR:
			e = w.readdir(d, a[1], a[2])
		case fn == fnClosedir:
			r, e = w.fileCall(d, sysClose, a)
		case d.sock != nil:
			op, b := fdArgs(fn, a)
			r, e = w.sockCall(d, op, b)
		default:
			op, b := fdArgs(fn, a)
			r, e = w.fileCall(d, op, b)
		}
		return result(fn, r, e)

	case fn <= fnChroot:
		op, b := pathArgs(fn, a)
		switch op {
		case sysGetcwd, sysChdir, sysTruncate, sysStatfs, sysChroot:
			if inc == nil {
				return
			}
		default:
			if inc == nil && !isSimFD(b[0]) {
				return
			}
			if realPath(op, b) {
				if inc != nil {
					return raw(fn, trap, a)
				}
				return
			}
		}
		var ts *[2]syscall.Timespec
		if fn == fnUtimes {
			ts = utimes(a[1])
			b[2] = uintptr(unsafe.Pointer(ts))
		}
		r, e := w.pathCall(inc, op, b)
		runtime.KeepAlive(ts)
		if op == sysGetcwd && e == 0 {
			r = a[0] // the buffer
		}
		return result(fn, r, e)

	case fn == fnMmap:
		if !isSimFD(a[4]) {
			return
		}
		d, e := w.fd(inc, a[4])
		if e != 0 || d.file == nil {
			return result(fn, 0, eBADF)
		}
		r, e := w.mmap(d.file, d.inc, trap, a)
		return result(fn, r, e)

	case fn == fnMunmap:
		// The kernel unmaps the memory; forget the mapping first.
		if w.munmap(a[0], a[1]) || inc != nil {
			return raw(fn, trap, a)
		}
		return

	case fn == fnMsync:
		const msSync = 0x10 // MS_SYNC on darwin
		if e, found := w.msync(a[0], a[1], a[2]&msSync != 0); found {
			return result(fn, 0, e)
		}
		if inc != nil {
			return raw(fn, trap, a)
		}

	case fn == fnMadvise || fn == fnMprotect:
		if inc != nil {
			return raw(fn, trap, a)
		}

	case fn == fnGetpid:
		if inc != nil {
			return uintptr(inc.pid()), 0, 0, true
		}

	case fn == fnKqueue:
		if inc != nil {
			return result(fn, 0, w.noWatch("kqueue"))
		}

	case fn == fnSocket || fn == fnSocketpair:
		if inc == nil {
			if fn == fnSocket && w.noNodeSocket(a[0]) {
				return result(fn, 0, uintptr(syscall.EPERM))
			}
			return
		}
		op := uintptr(sysSocket)
		if fn == fnSocketpair {
			op = sysSocketpair
		}
		r, e := w.socketCall(inc, op, a)
		return result(fn, r, e)

	case fn == fnGetaddrinfo || fn == fnGetnameinfo:
		if inc != nil {
			const _EAI_NONAME = 8
			return _EAI_NONAME, 0, 0, true
		}
	case fn == fnResNinit || fn == fnResNsearch:
		if inc != nil {
			return ^uintptr(0), 0, 0, true
		}
	case fn == fnResNclose:
		if inc != nil {
			return 0, 0, 0, true // of the state res_9_ninit left alone
		}
	}
	return
}

// fdArgs translates a call on a descriptor to the kernel's form.
func fdArgs(fn libcFn, a [6]uintptr) (uintptr, [6]uintptr) {
	switch fn {
	case fnRead:
		return sysRead, a
	case fnWrite:
		return sysWrite, a
	case fnPread:
		return sysPread64, a
	case fnPwrite:
		return sysPwrite64, a
	case fnReadv:
		return sysReadv, a
	case fnWritev:
		return sysWritev, a
	case fnClose:
		return sysClose, a
	case fnFstat:
		return sysFstat, a
	case fnLseek:
		return sysLseek, a
	case fnFsync:
		return sysFsync, a
	case fnFtruncate:
		return sysFtruncate, a
	case fnFcntl:
		if a[1] == syscall.F_FULLFSYNC {
			return sysFsync, a
		}
		return sysFcntl, a
	case fnFlock:
		return sysFlock, a
	case fnFchmod:
		return sysFchmod, a
	case fnFchown:
		return sysFchown, a
	case fnFchdir:
		return sysFchdir, a
	case fnFstatfs:
		return sysFstatfs, a
	case fnDup2:
		return sysDup3, a
	case fnShutdown:
		return sysShutdown, a
	case fnGetsockopt:
		return sysGetsockopt, a
	case fnSetsockopt:
		return sysSetsockopt, a
	case fnBind:
		return sysBind, a
	case fnListen:
		return sysListen, a
	case fnAccept:
		return sysAccept4, [6]uintptr{a[0], a[1], a[2], 0}
	case fnConnect:
		return sysConnect, a
	case fnGetsockname:
		return sysGetsockname, a
	case fnGetpeername:
		return sysGetpeername, a
	case fnRecvfrom:
		return sysRecvfrom, a
	case fnSendto:
		return sysSendto, a
	case fnRecvmsg:
		return sysRecvmsg, a
	case fnSendmsg:
		return sysSendmsg, a
	}
	return 0, a
}

// pathArgs translates a call that names a path to the kernel's form.
func pathArgs(fn libcFn, a [6]uintptr) (uintptr, [6]uintptr) {
	const cwd = ^uintptr(-_AT_FDCWD - 1) // _AT_FDCWD as a uintptr
	switch fn {
	case fnOpen:
		return sysOpenat, [6]uintptr{cwd, a[0], a[1], a[2]}
	case fnOpenat:
		return sysOpenat, a
	case fnStat:
		return sysNewfstatat, [6]uintptr{cwd, a[0], a[1], 0}
	case fnLstat:
		return sysNewfstatat, [6]uintptr{cwd, a[0], a[1], _AT_SYMLINK_NOFOLLOW}
	case fnFstatat:
		return sysNewfstatat, a
	case fnMkdir:
		return sysMkdirat, [6]uintptr{cwd, a[0], a[1]}
	case fnMkdirat:
		return sysMkdirat, a
	case fnUnlink:
		return sysUnlinkat, [6]uintptr{cwd, a[0], 0}
	case fnRmdir:
		return sysUnlinkat, [6]uintptr{cwd, a[0], _AT_REMOVEDIR}
	case fnUnlinkat:
		return sysUnlinkat, a
	case fnRename:
		return sysRenameat, [6]uintptr{cwd, a[0], cwd, a[1]}
	case fnRenameat:
		return sysRenameat, a
	case fnRenameatxNp:
		return sysRenameat2, a
	case fnReadlink:
		return sysReadlinkat, [6]uintptr{cwd, a[0], a[1], a[2]}
	case fnReadlinkat:
		return sysReadlinkat, a
	case fnAccess:
		return sysFaccessat, [6]uintptr{cwd, a[0], a[1], 0}
	case fnFaccessat:
		return sysFaccessat, a
	case fnChmod:
		return sysFchmodat, [6]uintptr{cwd, a[0], a[1], 0}
	case fnFchmodat:
		return sysFchmodat, a
	case fnChown, fnLchown:
		return sysFchownat, [6]uintptr{cwd, a[0], a[1], a[2], 0}
	case fnFchownat:
		return sysFchownat, a
	case fnUtimes:
		return sysUtimensat, [6]uintptr{cwd, a[0], 0, 0} // times from utimes
	case fnUtimensat:
		return sysUtimensat, a
	case fnSymlink:
		return sysSymlinkat, [6]uintptr{a[0], cwd, a[1]}
	case fnSymlinkat:
		return sysSymlinkat, a
	case fnLink:
		return sysLinkat, [6]uintptr{cwd, a[0], cwd, a[1], 0}
	case fnLinkat:
		return sysLinkat, a
	case fnMknod, fnMkfifo:
		return sysMknodat, [6]uintptr{cwd, a[0], a[1], a[2]}
	case fnGetcwd:
		return sysGetcwd, a
	case fnChdir:
		return sysChdir, a
	case fnTruncate:
		return sysTruncate, a
	case fnStatfs:
		return sysStatfs, a
	case fnChroot:
		return sysChroot, a
	}
	return 0, a
}

// utimes converts the times of utimes, a pointer to two timevals or nil
// for now, to those of utimensat.
func utimes(tv uintptr) *[2]syscall.Timespec {
	if tv == 0 {
		return nil
	}
	t := (*[2]syscall.Timeval)(ptr(tv))
	return &[2]syscall.Timespec{syscall.NsecToTimespec(t[0].Nano()), syscall.NsecToTimespec(t[1].Nano())}
}

// realPrefixes are the read-only system files that nodes read from the
// real machine, where their contents are fixed: time zones and
// certificates.
var realPrefixes = []string{
	"/usr/share/zoneinfo/", "/var/db/timezone/", "/private/var/db/timezone/",
	"/etc/localtime", "/etc/ssl/", "/private/etc/ssl/", "/usr/local/etc/openssl",
	"/opt/homebrew/etc/openssl", "/etc/mime.types",
}

// mmap maps a simulated file; see the Linux version. Every mapping is
// anonymous memory holding a copy of the file, as Linux's private
// mappings are; a shared writable one has its stores written back as
// Linux's shared mappings do (see writeBackShm). trap is the caller's mmap
// trampoline, which maps the memory too.
func (w *World) mmap(f *openFile, inc *incarnation, trap uintptr, a [6]uintptr) (uintptr, uintptr) {
	n := f.ino
	if n.dir || a[3]&syscall.MAP_FIXED != 0 || int64(a[5]) < 0 || a[5]%4096 != 0 {
		return 0, uintptr(syscall.EINVAL)
	}
	shared := a[2]&syscall.PROT_WRITE != 0 && a[3]&syscall.MAP_SHARED != 0
	if shared && f.flags&syscall.O_ACCMODE != syscall.O_RDWR {
		return 0, uintptr(syscall.EACCES)
	}
	// The copy starts as the page cache is, with the stores to the file's
	// shared mappings.
	w.writeBackFile(n)
	// Writable, so that writes to the file can update it.
	addr, _, e := rawSyscall6(trap, nextMapAddr(a[1]), a[1], syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE|syscall.MAP_ANON, ^uintptr(0), 0)
	if addr == ^uintptr(0) {
		return 0, uintptr(e)
	}
	m := mapping{ino: n, disk: f.disk, inc: inc, off: int64(a[5]), addr: addr, len: a[1], prot: a[2], shared: shared}
	w.addMap(m)
	w.fillMapping(m)
	return addr, 0
}

// writeBackShm writes back the program's stores to n's shared mappings,
// one after the other: a page that two of them changed since the last
// write-back keeps the stores of the first.
func (w *World) writeBackShm(n *inode) {
	for _, m := range w.maps {
		if m.ino == n && m.shared {
			w.writeBackMem(n, mem(m.addr, m.len), m.off, nil)
		}
	}
}

// On macOS no file has a shmem.
type shmem struct{}

func (w *World) syncShm(*inode, int64, int64) {}
func (w *World) releaseShm(*inode)            {}
func forked()                                 {}
