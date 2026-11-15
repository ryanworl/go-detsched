// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build (linux && (amd64 || arm64)) || (darwin && !ios && arm64)

package sim

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/buggify"
)

// TestTruncateRetains checks which truncated files a disk keeps for its
// next metadata commit: on a journaling disk, every file truncated since,
// whose size another file's fsync commits; on a strict disk none, which
// otherwise kept every file a program truncated and removed, with its
// data, for as long as no directory fsync came.
func TestTruncateRetains(t *testing.T) {
	if !Supported() {
		t.Skip("requires GOEXPERIMENT=detsched")
	}
	var w World
	d := newDisk(1)
	file := func(name string) *inode {
		n := d.newInode(false, 0o644)
		d.setEnt(d.root, name, n)
		n.data = make([]byte, 8192)
		n.pending = []pwrite{{off: 0, data: n.data}}
		return n
	}

	a, b := file("a"), file("b")
	w.truncate(d, a, 100)
	if !d.truncated[a] {
		t.Fatalf("journaling disk: truncated file not kept for the next commit")
	}
	d.fsync(b)
	if len(d.truncated) != 0 || len(a.ddata) != 100 {
		t.Errorf("journaling disk: after another file's fsync, %d files kept, durable size %d; want 0, 100", len(d.truncated), len(a.ddata))
	}

	buggify.Force("sim.disk.strictdirsync", true)
	defer buggify.Force("sim.disk.strictdirsync", false)
	c := file("c")
	w.truncate(d, c, 0)
	d.setEnt(d.root, "c", nil)
	if len(d.truncated) != 0 {
		t.Errorf("strict disk: %d removed files kept after truncation; want 0", len(d.truncated))
	}
	e := file("e")
	w.truncate(d, e, 50)
	d.fsync(e)
	if len(e.ddata) != 50 {
		t.Errorf("strict disk: durable size after the file's own fsync is %d; want 50", len(e.ddata))
	}
}

// TestHoleNeedsBlocks checks that on a full disk a write fails with ENOSPC
// where the file has no blocks: past its end, or in the hole a growing
// truncation left; a write over data it wrote before succeeds.
func TestHoleNeedsBlocks(t *testing.T) {
	if !Supported() {
		t.Skip("requires GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := NewWorld(Config{})
		defer w.Close()
		n := w.AddNode("a", "10.0.0.1")
		res := make(chan []error, 1)
		n.Start(func() {
			f, err := os.Create("/f")
			if err != nil {
				res <- []error{err}
				return
			}
			f.Write(make([]byte, 4096))
			f.Truncate(1 << 20)
			n.SetDiskFull(true)
			var errs []error
			for _, off := range []int64{100, 8192, 2 << 20} {
				_, err := f.WriteAt([]byte("x"), off)
				errs = append(errs, err)
			}
			n.SetDiskFull(false)
			_, err = f.WriteAt([]byte("x"), 8192)
			errs = append(errs, err)
			res <- errs
		})
		errs := <-res
		want := []error{nil, syscall.ENOSPC, syscall.ENOSPC, nil}
		if len(errs) != len(want) {
			t.Fatalf("got %v, want %v", errs, want)
		}
		for i := range want {
			if !errors.Is(errs[i], want[i]) && !(errs[i] == nil && want[i] == nil) {
				t.Errorf("write %d: got %v, want %v", i, errs[i], want[i])
			}
		}
	})
}

// TestSyncErrorEachFile checks that an fsync failure is reported once to
// every file open on the inode when it failed, as Linux's errseq does: a
// program whose descriptor did not make the failing fsync must still learn
// that its earlier writes may be lost.
func TestSyncErrorEachFile(t *testing.T) {
	if !Supported() {
		t.Skip("requires GOEXPERIMENT=detsched")
	}
	d := newDisk(1)
	n := d.newInode(false, 0o644)
	d.setEnt(d.root, "f", n)
	a := &openFile{disk: d, ino: n, errSeen: n.errSeq}
	b := &openFile{disk: d, ino: n, errSeen: n.errSeq}
	n.pending = []pwrite{{off: 0, data: []byte("x")}}

	buggify.Force("sim.disk.fsyncfail", true)
	if e := d.syncFile(a); e != uintptr(syscall.EIO) {
		t.Fatalf("forced failing fsync through a: errno %d; want EIO", e)
	}
	buggify.Force("sim.disk.fsyncfail", false)
	c := &openFile{disk: d, ino: n, errSeen: n.errSeq} // opened after the failure
	for _, tc := range []struct {
		name string
		f    *openFile
		want uintptr
	}{
		{"a again", a, 0},
		{"b, open at the failure", b, uintptr(syscall.EIO)},
		{"b again", b, 0},
		{"c, opened after it", c, 0},
	} {
		if e := d.syncFile(tc.f); e != tc.want {
			t.Errorf("fsync through %s: errno %d; want %d", tc.name, e, tc.want)
		}
	}
}

// TestDescriptorRanges checks that file and socket descriptors stay in
// their own ranges after a process opened millions of files, as a run
// whose program retries a failing file operation does. Descriptor numbers
// are never reused; files once ran into the socket range after 4 million
// opens, replacing sockets of other nodes, and then past every simulated
// number, so the real kernel answered EBADF.
func TestDescriptorRanges(t *testing.T) {
	w := World{fds: map[int]*fdesc{}}
	f0, s0 := nextFile, nextSock
	defer func() { nextFile, nextSock = f0, s0 }()
	nextFile = fdFileBase + 1<<22 + 5
	sock := w.newFD(&fdesc{sock: &socket{}})
	file := w.newFD(&fdesc{file: &openFile{}})
	if !isSimFD(uintptr(file)) || file >= fdSockBase {
		t.Errorf("file descriptor %#x after %d opens is not in the file range [%#x, %#x)", file, 1<<22+5, fdFileBase, fdSockBase)
	}
	if !isSimFD(uintptr(sock)) || sock < fdSockBase || sock == file {
		t.Errorf("socket descriptor %#x is not in the socket range [%#x, %#x) apart from file %#x", sock, fdSockBase, fdLimit, file)
	}
}
