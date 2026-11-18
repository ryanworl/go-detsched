// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build (linux && (amd64 || arm64)) || (darwin && !ios && arm64)

package sim

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/buggify"
)

// The simulated disk.
//
// Each inode has its current state, which reads see, and its durable
// state, which survives a power failure. fsync of a file makes its data
// durable. Directory entries are metadata: by default fsync of any file or
// directory commits all metadata changes made before it, as an ordered
// journal does (ext4's data=ordered, XFS). With the opt-in buggify site
// "sim.disk.strictdirsync" (see buggify.Enable) active, only fsync of a
// directory commits that directory's entries, which is all POSIX promises:
// few programs fsync the directories they create files in. A rename
// between directories is one change to both, which fsync of either
// commits. Writes with O_SYNC or O_DSYNC are durable when they return.
//
// Calls that wait for the device make the calling goroutine wait in
// simulated time, while the node's other goroutines and the other nodes
// run on: fsync and fdatasync (F_FULLFSYNC on macOS), writes with O_SYNC
// or O_DSYNC, and msync with MS_SYNC. Each node's disk has a flush latency
// drawn from the fault stream when the node is added, from 50µs (NVMe
// with power-loss protection) to 20ms (a disk that seeks), and a write
// bandwidth from 50 MB/s to 2 GB/s. A sync waits half the latency plus
// an exponentially distributed time with the same mean, and the time the
// bandwidth takes to write the file's unsynced data. Reads and other
// writes find the page cache and do not wait. The sync takes effect when
// the call starts: a crash during the wait keeps what it synced, as a
// device may complete a write it has not acknowledged.
//
// Faults, as buggify sites:
//
//   - "sim.disk.partialcrash": a power failure keeps a random subset of the
//     writes made since each file's last fsync, as writeback may have
//     flushed some of them before the failure.
//   - "sim.disk.torn": as partialcrash, but each 512-byte sector of a write
//     survives or not on its own, so writes tear.
//   - "sim.disk.fsyncfail": fsync fails with EIO, with Linux's semantics
//     (fsyncgate): the dirty pages are dropped from the disk's view, so
//     reads still see the data, a power failure loses it, and the next
//     fsync succeeds without writing it, unless a later write dirtied
//     its page again: writeback writes whole pages. As with Linux's
//     errseq, every other file open on it when it failed reports the
//     failure once, at its next fsync.
//   - "sim.disk.readerror": a latent sector error: a sector a read covers
//     goes bad, and every read covering it fails with EIO until a write
//     covers the whole sector, as a drive remaps it, or truncation drops
//     it. Reads of pages with unsynced writes come from the page cache
//     and never fail.
//   - "sim.disk.slowsync": a sync stalls for 10ms to 5s more, as a
//     device's tail latency does (a garbage collection in an SSD, a
//     retried sector, a hiccup of network storage).
//   - "sim.disk.enospc": the disk is full for a while (100ms to 5s):
//     writes that need new blocks, and creating files and directories,
//     fail with ENOSPC. A file has blocks up to the end of its furthest
//     write or fallocate: a truncation that grows it leaves a hole, whose
//     writes need blocks, as on Linux. Node.SetDiskFull sets it
//     explicitly.
//
// Nemesis.StopFaults and Stop end a full disk and slow syncs, and clear
// latent errors. The nemesis's FaultDisk slows a node's syncs or stalls
// them until it ends.
//   - Opt-in (buggify.Enable), as few systems claim to survive them:
//     "sim.disk.bitflip" flips a bit in a written sector, and
//     "sim.disk.misdirect" writes data at another offset than asked, on
//     the platter: the program reads what it wrote until a power failure
//     empties the page cache.
//
// A harness for a system that treats a kind as fatal, such as one that
// stops on a failed write to its log, disables it with buggify.Disable.

type disk struct {
	dev     uint64
	root    *inode
	nextIno uint64

	truncated  map[*inode]bool // files truncated since the last commitMeta
	full       bool            // set by Node.SetDiskFull
	fullUntil  int64           // full because of "sim.disk.enospc" until then
	syncFailed bool            // an fsync failed since the last power failure
	syncExempt []string        // set by Node.SetSyncFaultExempt

	// The device's speed (drawDisk), and the nemesis's FaultDisk: syncs
	// take slow times as long while slow > 1, and wait until stallUntil.
	latency    int64 // mean flush latency, in ns
	bandwidth  int64 // in bytes per second
	slow       int64
	stallUntil int64

	// dirty holds every directory in the tree with changed entries (see
	// inode.changed) or no durable entries yet, so that commitMeta costs
	// what changed since the last commit, not the size of the tree. It
	// may hold directories out of the tree too, which commitMeta drops.
	dirty map[*inode]bool
}

const (
	sectorSize  = 512
	maxFileSize = 1 << 34 // larger writes and truncations fail with EFBIG
)

// checkSize reports whether a file can be truncated to size, or written up
// to it.
func checkSize(size int64) uintptr {
	switch {
	case size < 0:
		return uintptr(syscall.EINVAL)
	case size > maxFileSize:
		return uintptr(syscall.EFBIG)
	}
	return 0
}

// noSpace reports whether the disk is full now.
func (d *disk) noSpace() bool {
	if d.full || now() < d.fullUntil {
		return true
	}
	if fault("sim.disk.enospc", "fault: sim.disk.enospc", 0.001) {
		d.fullUntil = now() + 100e6 + int64(runtime_rand()%4900e6)
		world.logEvent(d.nodeName(), "disk full (ENOSPC) for", strconv.FormatFloat(float64(d.fullUntil-now())/1e9, 'f', 3, 64)+"s")
		return true
	}
	return false
}

type inode struct {
	ino   uint64
	dir   bool
	perm  uint32
	mtime int64

	data    []byte
	pending []pwrite // writes since the last fsync
	ddata   []byte   // durable data
	alloc   int64    // data has blocks up to here (see "sim.disk.enospc")
	shm     *shmem   // the memory its shared mappings map, or nil

	writable int // its mappings that are shared and writable (mapping.shared)
	private  int // its mappings that are private copies (not mapping.aliased)

	parent *inode
	name   string // a directory's name in its parent
	ents   map[string]*inode
	dents  map[string]*inode // durable entries, nil until first committed

	// changed holds the names whose entry may differ from the durable
	// one: every other name has the same entry, or none, in ents and
	// dents.
	changed map[string]bool

	bad map[int64]bool // sectors with latent errors

	// errSeq counts the fsync failures of the file, as Linux's errseq_t
	// of its mapping: each open file sees a failure once, at its next
	// fsync, whichever descriptor's fsync failed.
	errSeq uint32

	// locks are the flock locks (locks[lockFlock]) and the open file
	// description locks (locks[lockOFD]) on the file, by the open file
	// that holds each: true for an exclusive lock. They cover the whole
	// file, end when the file is closed, and, as in Linux, the two kinds
	// do not interact.
	locks [2]map[*openFile]bool
}

type pwrite struct {
	off   int64
	data  []byte
	trunc bool // a truncation to off
}

type openFile struct {
	disk    *disk
	ino     *inode
	off     int64
	flags   int
	errSeen uint32   // ino.errSeq this file reported last (see inode.errSeq)
	names   []string // directory listing, taken by the first getdents
	listed  bool
}

func newDisk(dev int) *disk {
	d := &disk{dev: uint64(dev), nextIno: 2, dirty: map[*inode]bool{}}
	d.root = &inode{ino: 1, dir: true, perm: 0o755, ents: map[string]*inode{}}
	d.root.parent = d.root
	d.dirty[d.root] = true
	tmp := d.newInode(true, 0o1777)
	tmp.parent, tmp.name = d.root, "tmp"
	d.setEnt(d.root, "tmp", tmp)
	d.commitMeta()
	return d
}

// drawDisk draws the speed of a new node's disk from the fault stream:
// a flush latency from 50µs to 20ms and a bandwidth from 50 MB/s to
// 2 GB/s, log-uniformly.
func (d *disk) drawDisk() {
	d.latency = int64(logUniform(50*time.Microsecond, 20*time.Millisecond))
	d.bandwidth = int64(logUniform(50e6, 2e9)) // a Duration, as a number
}

// wait makes the calling goroutine wait for the device to write n's
// unsynced data and flush it, once the system call returns: at d's speed,
// times a slow FaultDisk, until a stalling one ends, and with
// "sim.disk.slowsync" longer.
func (d *disk) wait(n *inode) {
	if d.latency == 0 {
		return // a disk of no node, in a test
	}
	bytes := 0
	for _, w := range n.pending {
		bytes += len(w.data)
	}
	t := d.latency/2 + int64(expRand(time.Duration(d.latency/2))) + int64(float64(bytes)/float64(d.bandwidth)*1e9)
	if d.slow > 1 {
		t *= d.slow
	}
	if left := d.stallUntil - now(); left > 0 {
		t += left
	}
	if fault("sim.disk.slowsync", "fault: sim.disk.slowsync", 0.01) {
		stall := logUniform(10*time.Millisecond, 5*time.Second)
		world.logEvent(d.nodeName(), "slow sync", stall.Round(time.Millisecond).String())
		t += int64(stall)
	}
	runtime_wait(t)
}

// strict reports whether only directory fsyncs commit directory entries,
// which each branch point decides again.
func strict() bool { return buggify.OptIn("sim.disk.strictdirsync") }

func (d *disk) newInode(dir bool, perm uint32) *inode {
	n := &inode{ino: d.nextIno, dir: dir, perm: perm, mtime: wallNow()}
	d.nextIno++
	if dir {
		n.ents = map[string]*inode{}
		d.dirty[n] = true // for commitMeta to give it durable entries
	}
	return n
}

// setEnt sets the entry name in dir to n, or removes it if n is nil.
func (d *disk) setEnt(dir *inode, name string, n *inode) {
	if n == nil {
		delete(dir.ents, name)
	} else {
		dir.ents[name] = n
	}
	d.change(dir, name)
}

// change notes that the entry name in dir may differ from its durable
// entry.
func (d *disk) change(dir *inode, name string) {
	if dir.changed == nil {
		dir.changed = map[string]bool{}
	}
	dir.changed[name] = true
	d.dirty[dir] = true
}

// inTree reports whether dir is in the disk's tree. A directory has one
// entry, which its parent and name give.
func (d *disk) inTree(dir *inode) bool {
	for ; dir != d.root; dir = dir.parent {
		if dir.parent.ents[dir.name] != dir {
			return false
		}
	}
	return true
}

// commitMeta makes all metadata durable: directory entries, and the
// sizes that truncations set. Truncating a file's durable data to each
// size it was truncated to since its last fsync, in order, gives the
// same contents as the file's next fsync or a power failure that keeps
// some of its unsynced writes would: a truncation erases every byte at
// or beyond its size, whatever wrote it before.
//
// It commits the entries of the directories in the tree that changed
// since they were last committed. A directory out of the tree, such as a
// removed one, keeps its durable entries and its changes, should a rename
// bring it back (see relist).
func (d *disk) commitMeta() {
	for n := range d.truncated {
		for _, w := range n.pending {
			if w.trunc {
				n.ddata = applyWrite(n.ddata, w)
			}
		}
	}
	clear(d.truncated)
	// Each directory commits on its own, so the order does not matter.
	for n := range d.dirty {
		if d.inTree(n) {
			if n.dents == nil {
				n.dents = make(map[string]*inode, len(n.ents))
			}
			for name := range n.changed {
				if c := n.ents[name]; c != nil {
					n.dents[name] = c
				} else {
					delete(n.dents, name)
				}
			}
			n.changed = nil
		}
		delete(d.dirty, n)
	}
}

// relist puts the directories under dir with uncommitted entries back in
// d.dirty, as a rename brings dir into the tree from a directory out of
// it, which a descriptor can still name.
func (d *disk) relist(dir *inode) {
	if dir.dents == nil || len(dir.changed) > 0 {
		d.dirty[dir] = true
	}
	for _, c := range dir.ents {
		if c.dir {
			d.relist(c)
		}
	}
}

func cloneEnts(m map[string]*inode) map[string]*inode {
	c := make(map[string]*inode, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// sync is fsync of n, which "sim.disk.fsyncfail" fails: the kernel drops
// the dirty pages from the disk's view and reports the failure once
// (fsyncgate).
func (d *disk) sync(n *inode) uintptr {
	d.wait(n)
	if !n.dir && len(n.pending) > 0 && !d.exempt(n) && fault("sim.disk.fsyncfail", "fault: sim.disk.fsyncfail", 0.05) {
		world.logEvent(d.nodeName(), "fsync failed (EIO): unsynced writes stay readable, a power failure loses them")
		n.pending = nil
		n.errSeq++
		d.syncFailed = true
		return uintptr(syscall.EIO)
	}
	d.fsync(n)
	if !n.dir && world != nil && world.setup {
		world.endSetup()
	}
	return 0
}

// syncFile is fsync through f: it fails as sync does, and otherwise reports
// a failure of an fsync through another file since f last reported one, as
// Linux reports a writeback error to every file open when it happened.
func (d *disk) syncFile(f *openFile) uintptr {
	n := f.ino
	e := d.sync(n)
	if e == 0 && f.errSeen != n.errSeq {
		e = uintptr(syscall.EIO)
	}
	f.errSeen = n.errSeq
	return e
}

// exempt reports whether a name of n ends with a suffix of
// Node.SetSyncFaultExempt.
func (d *disk) exempt(n *inode) bool {
	if len(d.syncExempt) == 0 {
		return false
	}
	var found func(dir *inode) bool
	found = func(dir *inode) bool {
		for name, c := range dir.ents {
			if c == n && slices.ContainsFunc(d.syncExempt, func(s string) bool { return strings.HasSuffix(name, s) }) || c.dir && found(c) {
				return true
			}
		}
		return false
	}
	return found(d.root)
}

// fsync makes n durable, and with it the metadata unless the disk is
// strict.
func (d *disk) fsync(n *inode) {
	if n.dir {
		if strict() {
			d.commitDir(n)
		} else {
			d.commitMeta()
		}
		return
	}
	for _, w := range n.pending {
		n.ddata = applyWrite(n.ddata, wholePages(n, w))
	}
	n.pending = nil
	delete(d.truncated, n) // its truncations are durable now
	if !strict() {
		d.commitMeta()
	}
}

// wholePages extends w to the pages it covers with the page cache's bytes
// around it, as writeback writes a dirty page whole: bytes of the page
// that an earlier fsync failed to write become durable with it.
func wholePages(n *inode, w pwrite) pwrite {
	end := w.off + int64(len(w.data))
	lo, hi := w.off&^(pageSize-1), min((end+pageSize-1)&^(pageSize-1), int64(len(n.data)))
	if w.trunc || end > hi || lo == w.off && end == hi {
		return w
	}
	data := make([]byte, 0, hi-lo)
	data = append(data, n.data[lo:w.off]...)
	data = append(data, w.data...)
	data = append(data, n.data[end:hi]...)
	return pwrite{off: lo, data: data}
}

// commitDir makes the entries of dir durable, and of the other side of a
// rename between it and another directory, which a journal commits as one
// change: a file renamed out of dir leaves it and joins its new directory
// together, and a file renamed into dir leaves its old one.
func (d *disk) commitDir(dir *inode) {
	added := map[*inode]bool{}   // durable in dir now, perhaps still elsewhere
	removed := map[*inode]bool{} // no longer in dir, perhaps not yet elsewhere
	for name, n := range dir.ents {
		if dir.dents[name] != n {
			added[n] = true
		}
	}
	for name, n := range dir.dents {
		if dir.ents[name] != n {
			removed[n] = true
		}
	}
	dir.dents = cloneEnts(dir.ents)
	dir.changed = nil
	delete(d.dirty, dir)
	if len(added) == 0 && len(removed) == 0 {
		return
	}
	seen := map[*inode]bool{}
	var walk func(p *inode)
	walk = func(p *inode) {
		if seen[p] {
			return
		}
		seen[p] = true
		if p != dir {
			// Each name fixed here has an entry and a durable entry
			// that differ, which p.changed already holds.
			for name, n := range p.dents {
				if added[n] && p.ents[name] != n {
					delete(p.dents, name) // renamed into dir
				}
			}
			for name, n := range p.ents {
				if removed[n] && p.dents[name] != n {
					if p.dents == nil {
						p.dents = map[string]*inode{} // p was never committed
					}
					p.dents[name] = n // renamed out of dir to here
				}
			}
		}
		for _, m := range []map[string]*inode{p.ents, p.dents} {
			for _, name := range sortedNames(m) {
				if c := m[name]; c.dir {
					walk(c)
				}
			}
		}
	}
	walk(d.root)
}

// endFaults ends the disk faults under way: a full disk (but not
// Node.SetDiskFull) and latent sector errors.
func (d *disk) endFaults() {
	d.fullUntil = 0
	seen := map[*inode]bool{}
	var walk func(n *inode)
	walk = func(n *inode) {
		if seen[n] {
			return
		}
		seen[n] = true
		n.bad = nil
		for _, m := range []map[string]*inode{n.ents, n.dents} {
			for _, c := range m {
				walk(c)
			}
		}
	}
	walk(d.root)
}

// pathOf returns a path of n on d, the first in order, or "a deleted file".
func (d *disk) pathOf(n *inode) string {
	var walk func(dir *inode, at string) string
	walk = func(dir *inode, at string) string {
		names := slices.Sorted(maps.Keys(dir.ents))
		for _, name := range names {
			if c := dir.ents[name]; c == n {
				return at + "/" + name
			}
		}
		for _, name := range names {
			if c := dir.ents[name]; c.dir {
				if p := walk(c, at+"/"+name); p != "" {
					return p
				}
			}
		}
		return ""
	}
	if p := walk(d.root, ""); p != "" {
		return p
	}
	return "a deleted file"
}

// nodeName returns the name of d's node, for World.logEvent.
func (d *disk) nodeName() string {
	if world != nil {
		for _, n := range world.nodes {
			if n.disk == d {
				return n.name
			}
		}
	}
	return "disk"
}

// powerFail reverts the disk to its durable state.
func (d *disk) powerFail() {
	d.syncFailed = false
	clear(d.truncated)
	partial := buggify.Active("sim.disk.partialcrash")
	torn := buggify.Active("sim.disk.torn")
	switch {
	case torn:
		world.logEvent(d.nodeName(), "power failure keeps some unsynced sectors")
	case partial:
		world.logEvent(d.nodeName(), "power failure keeps some unsynced writes")
	}
	var walk func(n, parent *inode)
	walk = func(n, parent *inode) {
		n.parent = parent
		if !n.dir {
			data := slices.Clone(n.ddata)
			kept := false // a write survived, so ddata changes too
			for _, w := range n.pending {
				switch {
				case torn && !w.trunc:
					for off := w.off; off < w.off+int64(len(w.data)); {
						end := min((off/sectorSize+1)*sectorSize, w.off+int64(len(w.data)))
						if buggify.Uint64()&1 == 0 {
							data = applyWrite(data, pwrite{off: off, data: w.data[off-w.off : end-w.off]})
							kept = true
						}
						off = end
					}
				case partial || torn:
					if buggify.Uint64()&1 == 0 {
						data = applyWrite(data, w)
						kept = true
					}
				}
			}
			n.data, n.pending = data, nil
			n.alloc = int64(len(data))
			if kept {
				n.ddata = slices.Clone(data)
			}
			return
		}
		if n.dents == nil {
			n.dents = map[string]*inode{}
		}
		n.ents = cloneEnts(n.dents)
		n.changed = nil
		for _, name := range sortedNames(n.ents) {
			c := n.ents[name]
			if c.dir {
				c.name = name
			}
			walk(c, n)
		}
	}
	walk(d.root, d.root)
	// The directories in the tree have all their entries durable now, and
	// those out of it keep theirs for relist.
	clear(d.dirty)
	d.fullUntil = 0
}

func applyWrite(data []byte, w pwrite) []byte {
	if w.trunc {
		if int64(len(data)) > w.off {
			return data[:w.off]
		}
		return append(data, make([]byte, w.off-int64(len(data)))...)
	}
	end := w.off + int64(len(w.data))
	if int64(len(data)) < end {
		data = append(data, make([]byte, end-int64(len(data)))...)
	}
	copy(data[w.off:], w.data)
	return data
}

func sortedNames(m map[string]*inode) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}

// lookup resolves path from dir. With parent set, it resolves all but the
// last element and returns the directory and the last element.
func lookup(dir *inode, root *inode, path string, parent bool) (*inode, string, uintptr) {
	if path == "" {
		return nil, "", uintptr(syscall.ENOENT)
	}
	if path[0] == '/' {
		dir = root
	}
	elems := strings.Split(path, "/")
	var clean []string
	for _, e := range elems {
		if e != "" {
			clean = append(clean, e)
		}
	}
	last := ""
	if parent {
		if len(clean) == 0 {
			return dir, ".", 0
		}
		last = clean[len(clean)-1]
		clean = clean[:len(clean)-1]
	}
	for _, e := range clean {
		if !dir.dir {
			return nil, "", uintptr(syscall.ENOTDIR)
		}
		switch e {
		case ".":
		case "..":
			dir = dir.parent
		default:
			next := dir.ents[e]
			if next == nil {
				return nil, "", uintptr(syscall.ENOENT)
			}
			dir = next
		}
	}
	if parent && !dir.dir {
		return nil, "", uintptr(syscall.ENOTDIR)
	}
	return dir, last, 0
}

// child returns the entry name in dir, resolving "." and "..".
func child(dir *inode, name string) *inode {
	switch name {
	case ".":
		return dir
	case "..":
		return dir.parent
	}
	return dir.ents[name]
}

// startDir returns the directory a path relative to dirfd starts at.
func (w *World) startDir(inc *incarnation, dirfd uintptr) (*disk, *inode, uintptr) {
	if int32(dirfd) == _AT_FDCWD {
		if inc == nil {
			return nil, nil, eBADF
		}
		return inc.node.disk, inc.cwd, 0
	}
	d := w.fds[int(dirfd)]
	if d == nil || d.file == nil {
		return nil, nil, eBADF
	}
	if !d.file.ino.dir {
		return nil, nil, uintptr(syscall.ENOTDIR)
	}
	return d.file.disk, d.file.ino, 0
}

// pathCall handles the system calls that name a path.
func (w *World) pathCall(inc *incarnation, trap uintptr, a [6]uintptr) (uintptr, uintptr) {
	if inc != nil && !inc.alive {
		return 0, uintptr(syscall.EIO)
	}
	switch trap {
	case sysGetcwd:
		path := pathOf(inc.cwd)
		buf := mem(a[0], a[1])
		if len(path)+1 > len(buf) {
			return 0, uintptr(syscall.ERANGE)
		}
		copy(buf, path)
		buf[len(path)] = 0
		return uintptr(len(path) + 1), 0
	case sysChdir:
		path, _ := cstring(a[0])
		n, _, e := lookup(inc.cwd, inc.node.disk.root, path, false)
		if e != 0 {
			return 0, e
		}
		if !n.dir {
			return 0, uintptr(syscall.ENOTDIR)
		}
		inc.cwd = n
		return 0, 0
	case sysTruncate:
		path, _ := cstring(a[0])
		n, _, e := lookup(inc.cwd, inc.node.disk.root, path, false)
		if e != 0 {
			return 0, e
		}
		if n.dir {
			return 0, uintptr(syscall.EISDIR)
		}
		if e := checkSize(int64(a[1])); e != 0 {
			return 0, e
		}
		w.truncate(inc.node.disk, n, int64(a[1]))
		return 0, 0
	case sysStatfs:
		path, _ := cstring(a[0])
		if _, _, e := lookup(inc.cwd, inc.node.disk.root, path, true); e != 0 {
			return 0, e
		}
		fillStatfs(inc.node.disk, ptr(a[1]))
		return 0, 0
	case sysChroot, sysStatx, sysOpenat2, sysInotifyAddWatch:
		return 0, eNOSYS
	case sysSymlinkat, sysMknodat:
		return 0, uintptr(syscall.EPERM)
	}

	dk, dir, e := w.startDir(inc, a[0])
	if e != 0 {
		return 0, e
	}
	path, ok := cstring(a[1])
	if !ok {
		return 0, uintptr(syscall.EFAULT)
	}
	switch trap {
	case sysOpenat:
		return w.openat(inc, dk, dir, path, int(a[2]), uint32(a[3]))

	case sysNewfstatat, sysFaccessat, sysFaccessat2,
		sysFchmodat, sysFchownat, sysUtimensat,
		sysReadlinkat:
		var n *inode
		if path == "" && (trap == sysNewfstatat && a[3]&_AT_EMPTY_PATH != 0) {
			n = dir
		} else {
			p, last, e := lookup(dir, dk.root, path, true)
			if e != 0 {
				return 0, e
			}
			if n = child(p, last); n == nil {
				return 0, uintptr(syscall.ENOENT)
			}
		}
		switch trap {
		case sysNewfstatat:
			fillStat(dk, n, (*syscall.Stat_t)(ptr(a[2])))
		case sysFchmodat:
			n.perm = uint32(a[2]) & 0o7777
		case sysUtimensat:
			if a[2] != 0 {
				ts := (*[2]syscall.Timespec)(ptr(a[2]))
				if ts[1].Nsec != _UTIME_OMIT {
					n.mtime = ts[1].Nano()
				}
			}
		case sysReadlinkat:
			return 0, uintptr(syscall.EINVAL)
		}
		return 0, 0

	case sysMkdirat:
		p, last, e := lookup(dir, dk.root, path, true)
		if e != 0 {
			return 0, e
		}
		if child(p, last) != nil {
			return 0, uintptr(syscall.EEXIST)
		}
		if dk.noSpace() {
			return 0, uintptr(syscall.ENOSPC)
		}
		n := dk.newInode(true, uint32(a[2])&0o7777)
		n.parent, n.name = p, last
		dk.setEnt(p, last, n)
		p.mtime = wallNow()
		return 0, 0

	case sysUnlinkat:
		p, last, e := lookup(dir, dk.root, path, true)
		if e != 0 {
			return 0, e
		}
		if last == "." || last == ".." {
			return 0, uintptr(syscall.EINVAL)
		}
		n := p.ents[last]
		if n == nil {
			return 0, uintptr(syscall.ENOENT)
		}
		if a[2]&_AT_REMOVEDIR != 0 {
			if !n.dir {
				return 0, uintptr(syscall.ENOTDIR)
			}
			if len(n.ents) > 0 {
				return 0, uintptr(syscall.ENOTEMPTY)
			}
		} else if n.dir {
			return 0, uintptr(syscall.EISDIR)
		}
		dk.setEnt(p, last, nil)
		p.mtime = wallNow()
		return 0, 0

	case sysLinkat:
		// A hard link is one more entry for the file's inode, durable
		// once its directory is synced, as a rename's is.
		if a[4]&^_AT_SYMLINK_FOLLOW != 0 {
			return 0, uintptr(syscall.EINVAL)
		}
		op, olast, e := lookup(dir, dk.root, path, true)
		if e != 0 {
			return 0, e
		}
		ndk, ndir, e := w.startDir(inc, a[2])
		if e != 0 {
			return 0, e
		}
		if ndk != dk {
			return 0, uintptr(syscall.EXDEV)
		}
		npath, _ := cstring(a[3])
		np, nlast, e := lookup(ndir, dk.root, npath, true)
		if e != 0 {
			return 0, e
		}
		n := child(op, olast)
		switch {
		case n == nil:
			return 0, uintptr(syscall.ENOENT)
		case n.dir:
			return 0, uintptr(syscall.EPERM)
		case child(np, nlast) != nil:
			return 0, uintptr(syscall.EEXIST)
		case dk.noSpace():
			return 0, uintptr(syscall.ENOSPC)
		}
		dk.setEnt(np, nlast, n)
		np.mtime = wallNow()
		return 0, 0

	case sysRenameat, sysRenameat2:
		if trap == sysRenameat2 && a[4] != 0 {
			return 0, uintptr(syscall.EINVAL)
		}
		op, olast, e := lookup(dir, dk.root, path, true)
		if e != 0 {
			return 0, e
		}
		ndk, ndir, e := w.startDir(inc, a[2])
		if e != 0 {
			return 0, e
		}
		if ndk != dk {
			return 0, uintptr(syscall.EXDEV)
		}
		npath, _ := cstring(a[3])
		np, nlast, e := lookup(ndir, dk.root, npath, true)
		if e != 0 {
			return 0, e
		}
		n := op.ents[olast]
		if n == nil {
			return 0, uintptr(syscall.ENOENT)
		}
		if old := np.ents[nlast]; old != nil && old != n {
			switch {
			case old.dir && !n.dir:
				return 0, uintptr(syscall.EISDIR)
			case !old.dir && n.dir:
				return 0, uintptr(syscall.ENOTDIR)
			case old.dir && len(old.ents) > 0:
				return 0, uintptr(syscall.ENOTEMPTY)
			}
		}
		for p := np; n.dir; p = p.parent {
			if p == n {
				return 0, uintptr(syscall.EINVAL) // into its own subtree
			}
			if p == dk.root {
				break
			}
		}
		dk.setEnt(op, olast, nil)
		dk.setEnt(np, nlast, n)
		if n.dir {
			n.parent, n.name = np, nlast
			if !dk.inTree(op) {
				dk.relist(n)
			}
		}
		op.mtime, np.mtime = wallNow(), wallNow()
		return 0, 0
	}
	return 0, eNOSYS
}

func (w *World) openat(inc *incarnation, dk *disk, dir *inode, path string, flags int, perm uint32) (uintptr, uintptr) {
	p, last, e := lookup(dir, dk.root, path, true)
	if e != 0 {
		return 0, e
	}
	n := child(p, last)
	if n == nil {
		if flags&syscall.O_CREAT == 0 {
			return 0, uintptr(syscall.ENOENT)
		}
		if dk.noSpace() {
			return 0, uintptr(syscall.ENOSPC)
		}
		n = dk.newInode(false, perm&0o7777)
		dk.setEnt(p, last, n)
		p.mtime = wallNow()
	} else if flags&(syscall.O_CREAT|syscall.O_EXCL) == syscall.O_CREAT|syscall.O_EXCL {
		return 0, uintptr(syscall.EEXIST)
	}
	if flags&syscall.O_DIRECTORY != 0 && !n.dir {
		return 0, uintptr(syscall.ENOTDIR)
	}
	if n.dir && flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0 {
		return 0, uintptr(syscall.EISDIR)
	}
	if !n.dir && flags&syscall.O_TRUNC != 0 && flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0 {
		w.truncate(dk, n, 0)
	}
	return uintptr(w.newFD(&fdesc{inc: w.ownerOf(inc, dk), file: &openFile{disk: dk, ino: n, flags: flags, errSeen: n.errSeq}})), 0
}

// ownerOf returns the incarnation that owns descriptors opened on dk.
func (w *World) ownerOf(inc *incarnation, dk *disk) *incarnation {
	if inc != nil {
		return inc
	}
	for _, n := range w.nodes {
		if n.disk == dk {
			return n.inc
		}
	}
	return nil
}

// truncate sets n's size. The new size is metadata, as a journaling file
// system logs it: an fsync of any file commits it unless the disk is
// strict (see commitMeta), while the data written before it stays
// unsynced. On a strict disk only an fsync of n commits it, so d does
// not keep n, which commitMeta would rarely if ever release.
func (w *World) truncate(d *disk, n *inode, size int64) {
	w.writeBackFile(n)
	old := int64(len(n.data))
	n.data = applyWrite(n.data, pwrite{off: size, trunc: true})
	n.pending = append(n.pending, pwrite{off: size, trunc: true})
	n.alloc = min(n.alloc, size)
	if !strict() {
		if d.truncated == nil {
			d.truncated = map[*inode]bool{}
		}
		d.truncated[n] = true
	}
	n.mtime = wallNow()
	w.mapped(n, min(old, size), max(old, size))
	for s := range n.bad {
		if s*sectorSize >= size {
			delete(n.bad, s)
		}
	}
}

func pathOf(n *inode) string {
	var elems []string
	for n.parent != n {
		p := n.parent
		for name, c := range p.ents {
			if c == n {
				elems = append(elems, name)
				break
			}
		}
		n = p
	}
	slices.Reverse(elems)
	return "/" + strings.Join(elems, "/")
}

// fileCall handles the system calls on a file or directory descriptor.
func (w *World) fileCall(d *fdesc, trap uintptr, a [6]uintptr) (uintptr, uintptr) {
	f := d.file
	n := f.ino
	acc := f.flags & syscall.O_ACCMODE
	w.writeBackFile(n)
	switch trap {
	case sysClose:
		delete(w.fds, int(a[0]))
		n.unlock(f)
		return 0, 0
	case sysRead, sysPread64, sysReadv:
		if n.dir {
			return 0, uintptr(syscall.EISDIR)
		}
		if acc == syscall.O_WRONLY {
			return 0, eBADF
		}
		var bufs [][]byte
		off := f.off
		switch trap {
		case sysReadv:
			bufs = iovecs(a[1], a[2])
		case sysPread64:
			bufs = [][]byte{mem(a[1], a[2])}
			off = int64(a[3])
			if off < 0 {
				return 0, uintptr(syscall.EINVAL)
			}
		default:
			bufs = [][]byte{mem(a[1], a[2])}
		}
		size := 0
		for _, b := range bufs {
			size += len(b)
		}
		if e := f.disk.readSectors(n, off, min(off+int64(size), int64(len(n.data)))); e != 0 {
			return 0, e
		}
		total := 0
		for _, b := range bufs {
			if off >= int64(len(n.data)) {
				break
			}
			m := copy(b, n.data[off:])
			off += int64(m)
			total += m
		}
		if trap != sysPread64 {
			f.off = off
		}
		return uintptr(total), 0
	case sysWrite, sysPwrite64, sysWritev:
		if n.dir {
			return 0, uintptr(syscall.EISDIR)
		}
		if acc == syscall.O_RDONLY {
			return 0, eBADF
		}
		var bufs [][]byte
		off := f.off
		switch trap {
		case sysWritev:
			bufs = iovecs(a[1], a[2])
		case sysPwrite64:
			bufs = [][]byte{mem(a[1], a[2])}
			off = int64(a[3])
		default:
			bufs = [][]byte{mem(a[1], a[2])}
		}
		if f.flags&syscall.O_APPEND != 0 && trap != sysPwrite64 {
			off = int64(len(n.data))
		}
		size := 0
		for _, b := range bufs {
			size += len(b)
		}
		if off < 0 {
			return 0, uintptr(syscall.EINVAL)
		}
		if e := checkSize(off + int64(size)); e != 0 {
			return 0, e
		}
		if off+int64(size) > n.alloc && f.disk.noSpace() {
			return 0, uintptr(syscall.ENOSPC)
		}
		n.alloc = max(n.alloc, off+int64(size))
		total := 0
		for _, b := range bufs {
			wr := pwrite{off: off, data: slices.Clone(b)}
			n.wroteSectors(wr.off, wr.off+int64(len(b)))
			n.data = applyWrite(n.data, wr)
			n.pending = append(n.pending, corrupt(wr))
			w.mapped(n, wr.off, wr.off+int64(len(wr.data)))
			off += int64(len(b))
			total += len(b)
		}
		n.mtime = wallNow()
		if trap != sysPwrite64 {
			f.off = off
		}
		if f.flags&(syscall.O_SYNC|syscall.O_DSYNC) != 0 {
			if e := f.disk.syncFile(f); e != 0 {
				return 0, e
			}
		}
		return uintptr(total), 0
	case sysLseek:
		var base int64
		switch a[2] {
		case 0:
		case 1:
			base = f.off
		case 2:
			base = int64(len(n.data))
		default:
			return 0, uintptr(syscall.EINVAL)
		}
		off := base + int64(a[1])
		if off < 0 {
			return 0, uintptr(syscall.EINVAL)
		}
		f.off = off
		if n.dir {
			f.listed = false
		}
		return uintptr(off), 0
	case sysFstat:
		fillStat(f.disk, n, (*syscall.Stat_t)(ptr(a[1])))
		return 0, 0
	case sysFsync, sysFdatasync:
		return 0, f.disk.syncFile(f)
	case sysFlock:
		const lockSH, lockEX, lockNB, lockUN = 1, 2, 4, 8
		switch op := a[1] &^ lockNB; op {
		case lockUN:
			delete(n.locks[lockFlock], f)
			return 0, 0
		case lockSH, lockEX:
			return 0, n.lock(f, lockFlock, op == lockEX, a[1]&lockNB == 0)
		}
		return 0, uintptr(syscall.EINVAL)
	case sysSyncFileRange, sysFchown:
		return 0, 0
	case sysFstatfs:
		fillStatfs(f.disk, ptr(a[1]))
		return 0, 0
	case sysFallocate:
		// Allocation itself is not simulated: mode 0 extends the file
		// as a truncation would, and FALLOC_FL_KEEP_SIZE does nothing.
		mode, off, size := a[1], int64(a[2]), int64(a[3])
		switch {
		case n.dir:
			return 0, uintptr(syscall.ENODEV)
		case acc == syscall.O_RDONLY:
			return 0, eBADF
		case off < 0 || size <= 0:
			return 0, uintptr(syscall.EINVAL)
		case mode&^1 != 0: // anything but FALLOC_FL_KEEP_SIZE
			return 0, uintptr(syscall.EOPNOTSUPP)
		}
		if e := checkSize(off + size); e != 0 {
			return 0, e
		}
		if f.disk.noSpace() {
			return 0, uintptr(syscall.ENOSPC)
		}
		n.alloc = max(n.alloc, off+size)
		if mode == 0 && off+size > int64(len(n.data)) {
			w.truncate(f.disk, n, off+size)
		}
		return 0, 0
	case sysFchmod:
		n.perm = uint32(a[1]) & 0o7777
		return 0, 0
	case sysFtruncate:
		if n.dir || acc == syscall.O_RDONLY {
			return 0, uintptr(syscall.EINVAL)
		}
		if e := checkSize(int64(a[1])); e != 0 {
			return 0, e
		}
		w.truncate(f.disk, n, int64(a[1]))
		return 0, 0
	case sysFchdir:
		if !n.dir {
			return 0, uintptr(syscall.ENOTDIR)
		}
		if d.inc != nil {
			d.inc.cwd = n
		}
		return 0, 0
	case sysFcntl:
		switch a[1] {
		case syscall.F_GETFL:
			return uintptr(f.flags), 0
		case syscall.F_SETFL:
			f.flags = f.flags&^(syscall.O_APPEND|syscall.O_NONBLOCK) | int(a[2])&(syscall.O_APPEND|syscall.O_NONBLOCK)
			return 0, 0
		case syscall.F_GETFD, syscall.F_SETFD:
			return 0, 0
		case syscall.F_SETLK, syscall.F_SETLKW:
			// A node is one process and its disk is its own, so
			// its record locks, which belong to the process, never
			// conflict: every lock succeeds.
			return 0, 0
		case syscall.F_GETLK:
			(*syscall.Flock_t)(ptr(a[2])).Type = syscall.F_UNLCK
			return 0, 0
		case _F_OFD_SETLK, _F_OFD_SETLKW:
			// Linux's open file description locks belong to the
			// open file, as flock's do, so two opens of a file in
			// one process conflict; a store may rely on it to keep
			// a cleaner from removing log files in use.
			switch fl := (*syscall.Flock_t)(ptr(a[2])); fl.Type {
			case syscall.F_UNLCK:
				delete(n.locks[lockOFD], f)
				return 0, 0
			case syscall.F_RDLCK, syscall.F_WRLCK:
				return 0, n.lock(f, lockOFD, fl.Type == syscall.F_WRLCK, a[1] == _F_OFD_SETLKW)
			}
			return 0, uintptr(syscall.EINVAL)
		case _F_OFD_GETLK:
			fl := (*syscall.Flock_t)(ptr(a[2]))
			if n.conflicts(f, lockOFD, fl.Type == syscall.F_WRLCK) {
				fl.Type, fl.Pid = syscall.F_WRLCK, -1
			} else {
				fl.Type = syscall.F_UNLCK
			}
			return 0, 0
		}
		return 0, uintptr(syscall.EINVAL)
	case sysGetdents64:
		if !n.dir {
			return 0, uintptr(syscall.ENOTDIR)
		}
		if !f.listed {
			f.names = append([]string{".", ".."}, sortedNames(n.ents)...)
			f.listed = true
			f.off = 0
		}
		buf := mem(a[1], a[2])
		used := 0
		for f.off < int64(len(f.names)) {
			name := f.names[f.off]
			c := child(n, name)
			if c == nil {
				f.off++ // removed since the listing
				continue
			}
			reclen := (19 + len(name) + 1 + 7) &^ 7
			if used+reclen > len(buf) {
				if used == 0 {
					return 0, uintptr(syscall.EINVAL)
				}
				break
			}
			rec := buf[used : used+reclen]
			clear(rec)
			*(*uint64)(unsafe.Pointer(&rec[0])) = c.ino
			*(*int64)(unsafe.Pointer(&rec[8])) = f.off + 1
			*(*uint16)(unsafe.Pointer(&rec[16])) = uint16(reclen)
			rec[18] = syscall.DT_REG
			if c.dir {
				rec[18] = syscall.DT_DIR
			}
			copy(rec[19:], name)
			used += reclen
			f.off++
		}
		return uintptr(used), 0
	case sysDup, sysDup3:
		return 0, uintptr(syscall.EMFILE)
	}
	return 0, eNOSYS
}

// The kinds of inode.locks.
const (
	lockFlock = iota
	lockOFD
)

// conflicts reports whether an open file other than f holds a lock of
// kind on n that conflicts with a shared, or with an exclusive, lock.
func (n *inode) conflicts(f *openFile, kind int, excl bool) bool {
	// No early return: the map's order depends on heap addresses, and
	// the work done must not.
	c := false
	for o, ex := range n.locks[kind] {
		c = c || o != f && (ex || excl)
	}
	return c
}

// lock takes a lock of kind on n for f, or reports EWOULDBLOCK if another
// open file holds a conflicting one. The simulation does not make a call
// wait for a lock: one that would reports EDEADLK, as the kernel does for
// a deadlock it detects, so that the program sees the conflict.
func (n *inode) lock(f *openFile, kind int, excl, wait bool) uintptr {
	if n.conflicts(f, kind, excl) {
		if wait {
			return uintptr(syscall.EDEADLK)
		}
		return uintptr(syscall.EWOULDBLOCK)
	}
	if n.locks[kind] == nil {
		n.locks[kind] = map[*openFile]bool{}
	}
	n.locks[kind][f] = excl
	return 0
}

// unlock releases f's locks on n, as closing f does.
func (n *inode) unlock(f *openFile) {
	delete(n.locks[lockFlock], f)
	delete(n.locks[lockOFD], f)
}

// readSectors reports EIO if the range [off, end) covers a bad sector that
// the read reaches the disk for, and with "sim.disk.readerror" makes a
// sector of it go bad.
func (d *disk) readSectors(n *inode, off, end int64) uintptr {
	if off >= end {
		return 0
	}
	if fault("sim.disk.readerror", "fault: sim.disk.readerror", 0.0005) {
		first, last := off/sectorSize, (end-1)/sectorSize
		if s := first + int64(runtime_rand()%uint64(last-first+1)); !n.cached(s) {
			if n.bad == nil {
				n.bad = map[int64]bool{}
			}
			n.bad[s] = true
			world.logEvent(d.nodeName(), "sector", strconv.FormatInt(s, 10), "of", d.pathOf(n), "went bad: reads of it fail with EIO until it is written")
		}
	}
	for s := off / sectorSize; s <= (end-1)/sectorSize && len(n.bad) > 0; s++ {
		if n.bad[s] && !n.cached(s) {
			return uintptr(syscall.EIO)
		}
	}
	return 0
}

// cached reports whether sector s is in a page with unsynced writes, which
// reads find in the page cache.
func (n *inode) cached(s int64) bool {
	const pageSize = 4096
	page := s * sectorSize &^ (pageSize - 1)
	for _, w := range n.pending {
		if !w.trunc && w.off < page+pageSize && w.off+int64(len(w.data)) > page {
			return true
		}
	}
	return false
}

// wroteSectors clears the latent errors of the sectors [off, end) covers
// whole, as the drive remaps them.
func (n *inode) wroteSectors(off, end int64) {
	for s := (off + sectorSize - 1) / sectorSize; (s+1)*sectorSize <= end && len(n.bad) > 0; s++ {
		delete(n.bad, s)
	}
}

// corrupt returns a write as it reaches the platter, with the opt-in
// silent corruption faults applied.
func corrupt(wr pwrite) pwrite {
	if calm() {
		return wr
	}
	if len(wr.data) > 0 && buggify.OptIn("sim.disk.bitflip") && chance(0.001) {
		injected("fault: sim.disk.bitflip")
		wr.data = slices.Clone(wr.data)
		i := runtime_rand() % uint64(len(wr.data))
		wr.data[i] ^= 1 << (runtime_rand() % 8)
	}
	if buggify.OptIn("sim.disk.misdirect") && chance(0.0005) {
		injected("fault: sim.disk.misdirect")
		d := int64(1+runtime_rand()%4) * 4096
		if runtime_rand()&1 == 0 && wr.off >= d {
			d = -d
		}
		wr.off += d
	}
	return wr
}
