// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build (linux && (amd64 || arm64)) || (darwin && !ios && arm64)

package sim

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// The simulated kernel. handle runs on the system stack, called by the
// runtime for system calls from package syscall, so it and everything it
// calls must not block, panic, or use much stack, and must not call
// anything that takes a lock that may be held, such as fmt or os.
//
// The kernel speaks Linux's system call interface: pathCall, fileCall,
// sockCall, and socketCall take a system call (the sys* constants) and
// its arguments as Linux passes them. A frontend for each OS decodes the
// OS's calls for them: handle, the layouts of stat and sockaddr, and the
// OS's flag values (kernel_linux.go, fs_linux.go, net_linux.go).

// Simulated descriptors (runtime/detsim.go's detsimFDBase and
// detsimSockBase). The runtime treats descriptors from fdSockBase on as
// pollable sockets. Numbers are never reused, so the ranges bound the
// files and sockets a process opens in all: about 500 million files and
// 500 million sockets, more than a run opens before any timeout, and
// below 1<<31, as descriptors are C ints.
const (
	fdFileBase = 1 << 24
	fdSockBase = 1 << 29
	fdLimit    = 1 << 30
)

// fdesc is an open simulated descriptor.
type fdesc struct {
	inc  *incarnation // the incarnation that opened it
	file *openFile
	sock *socket
}

func isSimFD(fd uintptr) bool { return fd >= fdFileBase && fd < fdLimit }

const (
	eNOSYS = uintptr(syscall.ENOSYS)
	eBADF  = uintptr(syscall.EBADF)
)

// realPath reports whether a path call only reads one of the real system
// files of realPrefixes, or Go's own time zone database, and goes to the
// real OS.
func realPath(trap uintptr, a [6]uintptr) bool {
	switch trap {
	case sysOpenat:
		if a[2]&(syscall.O_ACCMODE|syscall.O_CREAT|syscall.O_TRUNC) != syscall.O_RDONLY {
			return false
		}
	case sysNewfstatat, sysFaccessat, sysFaccessat2, sysStatx, sysReadlinkat:
	default:
		return false
	}
	path, ok := cstring(a[1])
	if !ok || len(path) == 0 || path[0] != '/' {
		return false
	}
	if strings.HasSuffix(path, "/lib/time/zoneinfo.zip") {
		return true
	}
	for _, p := range realPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// noNodeSocket reports whether handle refuses a goroutine of no node,
// such as a client the test body started, the socket in domain it asks
// for. An Internet socket would be real: dialing or listening on it would
// wait on the real network, which stalls the deterministic scheduler and
// makes the run depend on the OS. The first refusal says so on standard
// error, written directly, as handle cannot print through package os.
func (w *World) noNodeSocket(domain uintptr) bool {
	if domain != syscall.AF_INET && domain != syscall.AF_INET6 {
		return false
	}
	if !w.noNodeMsg {
		w.noNodeMsg = true
		const msg = "sim: a goroutine that belongs to no simulated node, such as one the test started, opened a network socket while a World is active; " +
			"it would use the real network, which stalls the deterministic scheduler, so the call fails with EPERM. " +
			"Run the code on a node: start it with Node.Go, or as a node's program with Node.Start.\n"
		syscall.RawSyscall(syscall.SYS_WRITE, 2, uintptr(unsafe.Pointer(unsafe.StringData(msg))), uintptr(len(msg)))
	}
	return true
}

// noWatch refuses a node the call name, which creates a watcher of file
// system changes: inotify_init1 on Linux, kqueue on macOS. Its events
// would come from the real file system, which the node's disk is not, and
// a goroutine waiting for them would wait on the OS, which the scheduler
// cannot tell apart from a hang. The first refusal says so on standard
// error, written directly, as noNodeSocket does.
func (w *World) noWatch(name string) uintptr {
	if !w.watchMsg {
		w.watchMsg = true
		msg := "sim: a node called " + name + ", which creates a watcher of file system changes (such as fsnotify.NewWatcher); " +
			"watchers are not simulated, and their events would come from the real file system, so the call fails with ENOSYS. " +
			"On nodes, notice changes another way, such as by polling with os.Stat, or turn the watcher off in the harness.\n"
		syscall.RawSyscall(syscall.SYS_WRITE, 2, uintptr(unsafe.Pointer(unsafe.StringData(msg))), uintptr(len(msg)))
	}
	return uintptr(syscall.ENOSYS)
}

// fd returns the descriptor fd, which a goroutine of incarnation inc, or
// of no node (nil), passes to the call handle is handling, or EBADF. A
// node's descriptors are its own, as a process's are: one that another
// incarnation opened, on another node or in an earlier start of the same
// node, is EBADF on the node, though all of them share one table here,
// and the first such use says so (see foreignFD). A goroutine of no node,
// such as the test's own, or the simulation's, uses any descriptor; a
// finalizer or cleanup set on a node runs on that node.
func (w *World) fd(inc *incarnation, fd uintptr) (*fdesc, uintptr) {
	d := w.fds[int(fd)]
	if inc == nil {
		if d == nil {
			return nil, eBADF
		}
		return d, 0
	}
	if d == nil {
		if owner := w.closedFDs[int(fd)]; owner != nil && owner != inc {
			w.foreignFD(inc, owner, fd, true)
		}
		return nil, eBADF
	}
	if d.inc != nil && d.inc != inc {
		w.foreignFD(inc, d.inc, fd, false)
		return nil, eBADF
	}
	return d, 0
}

// foreignFD says once, on standard error, that incarnation inc called
// w.call on descriptor fd, which owner opened, and which closed says a
// crash or stop of owner has closed since. Code often ignores the error
// of a call such as Close, so the message names both incarnations and
// the call. It is written directly, as noNodeSocket's is.
func (w *World) foreignFD(inc, owner *incarnation, fd uintptr, closed bool) {
	if w.fdMsg {
		return
	}
	w.fdMsg = true
	whose := "which node " + owner.name() + " opened"
	if closed {
		whose += " before it crashed or stopped"
	}
	msg := "sim: node " + inc.name() + " called " + callName(w.call) + " on descriptor " + strconv.Itoa(int(fd)) + ", " + whose + "; " +
		"each node's descriptors are its own, as a process's are, so the call fails with EBADF. " +
		"A harness that keeps an *os.File, a net.Conn, or a client that holds one where several nodes, or several starts of a node, reach it, " +
		"such as in a package variable, shares it between them: open it in the code each node runs instead.\n"
	syscall.RawSyscall(syscall.SYS_WRITE, 2, uintptr(unsafe.Pointer(unsafe.StringData(msg))), uintptr(len(msg)))
}

// The next descriptors newFD allocates. No number is used twice, not
// even by a later world: the runtime finds a simulated socket's poll
// descriptor by its number, and keeps those of the sockets that a world's
// crashed nodes still had open.
var nextFile, nextSock = fdFileBase, fdSockBase

// newFD allocates a descriptor for d.
func (w *World) newFD(d *fdesc) int {
	var fd int
	if d.sock != nil {
		fd = nextSock
		nextSock++
		d.sock.fd = fd
	} else {
		fd = nextFile
		nextFile++
	}
	w.fds[fd] = d
	return fd
}

// Access to the caller's memory. The runtime runs the handler on the
// system stack, so the caller's stack cannot move while it runs.

func mem(p uintptr, n uintptr) []byte {
	if p == 0 || n == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(ptr(p)), n)
}

func cstring(p uintptr) (string, bool) {
	if p == 0 {
		return "", false
	}
	for n := uintptr(0); n < 4096; n++ {
		if *(*byte)(ptr(p + n)) == 0 {
			return string(mem(p, n)), true
		}
	}
	return "", false
}

func putInt32(p uintptr, v int32) {
	if p != 0 {
		*(*int32)(ptr(p)) = v
	}
}

func getInt32(p uintptr) int32 {
	if p == 0 {
		return 0
	}
	return *(*int32)(ptr(p))
}

// iovecs returns the buffers of an iovec array.
func iovecs(p, n uintptr) [][]byte {
	var bufs [][]byte
	for i := uintptr(0); i < n && i < 1024; i++ {
		iov := (*syscall.Iovec)(ptr(p + i*unsafe.Sizeof(syscall.Iovec{})))
		bufs = append(bufs, mem(uintptr(unsafe.Pointer(iov.Base)), uintptr(iov.Len)))
	}
	return bufs
}

// crash ends inc. A process crash closes its descriptors as the kernel
// would, which keeps running; a power failure stops the kernel too, leaves
// all of the incarnation's sockets silent, also those still connecting,
// and loses its disk's unsynced data.
func (w *World) crash(inc *incarnation, process bool) {
	inc.alive = false
	for fd, d := range w.fds {
		if d.inc == inc {
			if process && d.sock != nil {
				w.sockClose(d.sock)
			}
			if d.file != nil {
				d.file.ino.unlock(d.file)
			}
			delete(w.fds, fd)
			if w.closedFDs == nil {
				w.closedFDs = make(map[int]*incarnation)
			}
			w.closedFDs[fd] = inc
		}
	}
	for s := range inc.socks {
		if s.udp != nil {
			if inc.uports[s.local.port] == s {
				delete(inc.uports, s.local.port)
			}
		} else if s.state == sListen || s.state == sBound {
			delete(inc.ports, s.local.port)
		}
		if !process {
			s.dead = true
		}
	}
	// The incarnation's memory goes with it, but its writes to shared
	// mappings are in the page cache, which survives a process crash
	// and which a power failure keeps some of, as it may have written
	// them back; after a power failure the other mappings of the disk's
	// files show what it kept.
	var done map[*inode]bool // written back
	for _, m := range w.maps {
		if m.inc == inc && (m.aliased || m.shared) && !done[m.ino] {
			if done == nil {
				done = map[*inode]bool{}
			}
			done[m.ino] = true
			w.writeBack(m)
		}
	}
	w.unmap(func(m mapping) bool { return m.inc == inc })
	if !process {
		inc.node.kernel = false
		if w.closing {
			delete(w.incs, inc.handle)
			return
		}
		inc.node.disk.powerFail()
		w.syncMaps(inc.node.disk)
	}
	delete(w.incs, inc.handle)
}

// ptr converts an address passed to a system call back to a pointer.
func ptr(p uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&p)) }

// A mapping is a simulated file mapped into memory. The memory is real.
// A private mapping is anonymous memory holding a copy of the file, which
// writes to the file update, as the page cache would. The shared
// mappings of a file all map one shared memory object, its shmem (see
// kernel_linux.go), which holds the file's bytes as the page cache does,
// so that a store through one shows through every other at once. On
// macOS a shared mapping is a copy too, and a store through it shows in
// the file and the file's other mappings once written back.
// The program's stores reach the file when the simulation writes them
// back (writeBack): before any call on a descriptor of the file, on
// msync and munmap, and when the process ends.
type mapping struct {
	ino       *inode
	disk      *disk
	inc       *incarnation // whose descriptor mapped it, or nil
	off       int64
	addr, len uintptr
	prot      uintptr
	shared    bool // MAP_SHARED and PROT_WRITE
	aliased   bool // MAP_SHARED: maps ino.shm
}

const pageSize = 4096

// writeBack writes back the program's stores to m, if it is shared, and
// those to every other shared mapping of its file.
func (w *World) writeBack(m mapping) {
	if m.aliased || m.shared {
		w.writeBackFile(m.ino)
	}
}

// writeBackFile writes back the program's stores to the shared mappings
// of n, if any is writable (see writeBackShm).
func (w *World) writeBackFile(n *inode) {
	if n.writable > 0 {
		w.writeBackShm(n)
	}
}

// msync handles msync of [addr, addr+length), and reports whether it
// covers a simulated mapping. With sync (MS_SYNC), it is an fsync of each
// file mapped there, which "sim.disk.fsyncfail" may fail.
func (w *World) msync(addr, length uintptr, sync bool) (errno uintptr, found bool) {
	for _, m := range w.maps {
		if m.addr < addr+length && addr < m.addr+m.len {
			found = true
			w.writeBack(m)
			if sync && m.disk != nil {
				if e := m.disk.sync(m.ino); e != 0 {
					errno = e
				}
			}
		}
	}
	return errno, found
}

// unmap unmaps the memory of the mappings f picks, and forgets them.
func (w *World) unmap(f func(mapping) bool) {
	w.maps = slices.DeleteFunc(w.maps, func(m mapping) bool {
		if !f(m) {
			return false
		}
		syscall.RawSyscall(syscall.SYS_MUNMAP, m.addr, m.len, 0)
		w.forget(m)
		return true
	})
}

// addMap adds m to the mappings, which are sorted by address.
func (w *World) addMap(m mapping) {
	i, _ := slices.BinarySearchFunc(w.maps, m.addr, func(o mapping, addr uintptr) int { return cmp.Compare(o.addr, addr) })
	w.maps = slices.Insert(w.maps, i, m)
	if m.shared {
		m.ino.writable++
	}
	if !m.aliased {
		m.ino.private++
	}
}

// forget drops m's hold on its file's shmem, which goes with the last.
func (w *World) forget(m mapping) {
	if m.shared {
		m.ino.writable--
	}
	if !m.aliased {
		m.ino.private--
	}
	if m.aliased {
		w.releaseShm(m.ino)
	}
}

// munmap writes back and forgets the mappings in [addr, addr+length) and
// reports whether there were any.
func (w *World) munmap(addr, length uintptr) bool {
	i, _ := slices.BinarySearchFunc(w.maps, addr, func(m mapping, addr uintptr) int { return cmp.Compare(m.addr, addr) })
	j := i
	for j < len(w.maps) && w.maps[j].addr < addr+length {
		w.writeBack(w.maps[j])
		j++
	}
	for _, m := range w.maps[i:j] {
		w.forget(m)
	}
	w.maps = slices.Delete(w.maps, i, j)
	return j > i
}

// mapped updates the mappings of n after a change to its bytes [off, end).
func (w *World) mapped(n *inode, off, end int64) {
	if n.shm != nil {
		w.syncShm(n, off, end)
	}
	if n.private == 0 {
		return
	}
	for _, m := range w.maps {
		if m.ino == n && !m.aliased {
			w.syncMapping(m, off, end)
		}
	}
}

// syncMaps updates the mappings of d's files after a power failure.
func (w *World) syncMaps(d *disk) {
	for _, m := range w.maps {
		switch {
		case m.disk != nil && m.disk != d:
		case m.aliased:
			w.syncShm(m.ino, 0, maxFileSize)
		default:
			w.syncMapping(m, m.off, m.off+int64(m.len))
		}
	}
}

// fillMapping copies the file into the fresh, zeroed memory of m, a
// private mapping, leaving the pages that would hold only zeros
// untouched, so that the kernel never populates them unless the program
// writes them.
func (w *World) fillMapping(m mapping) {
	n := m.ino
	end := min(m.off+int64(m.len), int64(len(n.data)))
	for off := m.off; off < end; off += pageSize {
		src := n.data[off:min(off+pageSize, end)]
		if !allZero(src) {
			copy(mem(m.addr+uintptr(off-m.off), uintptr(len(src))), src)
		}
	}
}

func allZero(b []byte) bool {
	for len(b) >= 8 {
		if *(*uint64)(unsafe.Pointer(&b[0])) != 0 {
			return false
		}
		b = b[8:]
	}
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func (w *World) syncMapping(m mapping, off, end int64) {
	lo, hi := max(off, m.off), min(end, m.off+int64(m.len))
	if lo >= hi {
		return
	}
	dst := mem(m.addr+uintptr(lo-m.off), uintptr(hi-lo))
	n := copy(dst, m.ino.data[min(lo, int64(len(m.ino.data))):])
	clear(dst[n:]) // past the end of the file
}

// mapNext is the address at which mmap maps the next file. The OS would
// place each mapping at an address that ASLR randomizes in every run, and
// programs depend on the addresses: package syscall keeps its mappings in
// a map keyed by address, whose layout, and so its allocations, the GC's
// timing, and the schedule, would change from run to run. Mappings count
// up from mapBase instead, far above the heap, whose base the runtime
// draws below 1<<46 from GODETSEED, and below where Linux places
// mappings, under the stack. Their addresses are never reused, and the
// runtime offers calls on them from any goroutine, such as a finalizer
// that unmaps one.
var mapNext uintptr = mapBase

const (
	mapBase  = 0x5000 << 32 // runtime.detsimMapBase
	mapAlign = 64 << 10     // a multiple of the page size on every system
)

// nextMapAddr returns the address at which to map length bytes.
func nextMapAddr(length uintptr) uintptr {
	hint := mapNext
	mapNext += (length + mapAlign - 1) &^ (mapAlign - 1)
	return hint
}

// zeroPage is a page of zeros, which a file holds past its data.
var zeroPage [pageSize]byte

// writeBackMem copies the program's stores in b, the memory of a mapping
// of n's bytes from off, into the file, as the kernel writes back a
// mapping's dirty pages: they become unsynced writes of the file, which
// reads see, fsync makes durable, and a power failure may lose, and they
// update n's private mappings. Each run of changed pages is one write,
// from the first changed byte to the last. Bytes past the end of the file
// are dropped. The pages of b for which written, if not nil, is false are
// known to be unchanged; the loop runs over them all the same, so that the schedule's
// ticks do not depend on what the kernel populated.
func (w *World) writeBackMem(n *inode, b []byte, off int64, written []bool) {
	end := min(int64(len(b)), n.size-off)
	if end <= 0 {
		return
	}
	lo, hi := int64(-1), int64(-1) // the run of changed bytes so far, in b
	flush := func() {
		if lo < 0 {
			return
		}
		wr := pwrite{off: off + lo, data: slices.Clone(b[lo:hi])}
		n.wroteSectors(wr.off, off+hi)
		n.data, _ = applyWrite(n.data, n.size, wr)
		n.alloc = max(n.alloc, off+hi) // a full disk would fault, which is not simulated
		n.pending = append(n.pending, corrupt(wr))
		n.mtime = wallNow()
		for i := 0; n.private > 0 && i < len(w.maps); i++ {
			if o := w.maps[i]; o.ino == n && !o.aliased && o.addr != uintptr(unsafe.Pointer(&b[0])) {
				w.syncMapping(o, off+lo, off+hi)
			}
		}
		lo, hi = -1, -1
	}
	for p := int64(0); p < end; {
		pe := min((p/pageSize+1)*pageSize, end)
		got, want := b[p:pe], zeroPage[:pe-p] // zeros past n.data
		if have := int64(len(n.data)); off+pe <= have {
			want = n.data[off+p : off+pe]
		} else if off+p < have {
			want = append(slices.Clone(n.data[off+p:]), zeroPage[:off+pe-have]...)
		}
		if written != nil && !written[p/pageSize] || string(got) == string(want) {
			flush()
			p = pe
			continue
		}
		i := 0
		for got[i] == want[i] {
			i++
		}
		j := len(got)
		for got[j-1] == want[j-1] {
			j--
		}
		if lo < 0 {
			lo = p + int64(i)
		}
		hi = p + int64(j)
		if j < len(got) {
			flush()
		}
		p = pe
	}
	flush()
}
