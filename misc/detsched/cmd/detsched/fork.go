// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"syscall"
)

// A forkServer drives a program running in fork mode (GODETSEARCH's fork; see
// runtime/detfork.go): it stops the program at switches and forks
// it into children that continue with a branch point added to their tape.
type forkServer struct {
	cmd  *exec.Cmd // nil for a forked child with its own control socket
	conn *net.UnixConn
	at   uint64 // switch the program is stopped at, or 0 when it ran to the end
}

// Commands and replies; see the constants in runtime/detfork.go.
const (
	forkRun    = 1
	forkBranch = 2
	forkWait   = 3
	forkExit   = 4

	forkStopped = 1
	forkPID     = 2
	forkStatus  = 3
	forkRefused = 4
	forkReady   = 5

	forkRunStopNovel = 1

	forkWithControl  = 1
	forkWithFeedback = 2
	forkWithSeen     = 4
)

// forkCgo is the reason a program with runtime.iscgo set gives, for
// every fork.
const forkCgo = 1

var forkReasons = map[uint64]string{
	forkCgo: "runtime.iscgo is set: the binary was built with cgo, or github.com/ebitengine/purego's internal/fakecgo sets it",
	2:       "the P is not on the main thread",
	3:       "an M that is locked to a goroutine, in a system call, or the template thread",
	4:       "descriptors registered with the network poller",
	5:       "already branched at this switch",
	6:       "bad descriptors",
	7:       "clone failed",
	8:       "the system's certificate verifier was used, and its connection to trustd does not survive fork",
}

// errRefused is returned by branch when the program cannot fork.
type errRefused struct{ reason uint64 }

func (e errRefused) Error() string {
	if r, ok := forkReasons[e.reason]; ok {
		return "fork refused: " + r
	}
	return fmt.Sprintf("fork refused: reason %d", e.reason)
}

// socketPair returns the two ends of a Unix socket pair (see
// controlSockets): ours as a UnixConn and theirs as a file.
func socketPair() (*net.UnixConn, *os.File, error) {
	fds, err := controlSockets()
	if err != nil {
		return nil, nil, err
	}
	ours := os.NewFile(uintptr(fds[0]), "fork control")
	defer ours.Close()
	c, err := net.FileConn(ours)
	if err != nil {
		syscall.Close(fds[1])
		return nil, nil, err
	}
	return c.(*net.UnixConn), os.NewFile(uintptr(fds[1]), "fork control"), nil
}

// startForkServer starts cmd in fork mode and waits for it to stop at its
// first switch. cmd's Env must be set.
func startForkServer(cmd *exec.Cmd) (*forkServer, error) {
	conn, theirs, err := socketPair()
	if err != nil {
		return nil, err
	}
	defer theirs.Close()
	cmd.ExtraFiles = []*os.File{theirs}
	cmd.Env = addSearch(cmd.Env, "fork=3")
	if err := cmd.Start(); err != nil {
		conn.Close()
		return nil, err
	}
	s := &forkServer{cmd: cmd, conn: conn}
	op, a1, _, err := s.recv()
	if err != nil || op != forkStopped {
		s.close()
		return nil, fmt.Errorf("fork server did not stop at its first switch (%d, %v)", op, err)
	}
	s.at = a1
	return s, nil
}

func (s *forkServer) send(op, a1, a2, a3 uint64, files ...*os.File) error {
	var b [32]byte
	binary.LittleEndian.PutUint64(b[0:], op)
	binary.LittleEndian.PutUint64(b[8:], a1)
	binary.LittleEndian.PutUint64(b[16:], a2)
	binary.LittleEndian.PutUint64(b[24:], a3)
	var oob []byte
	if len(files) > 0 {
		fds := make([]int, len(files))
		for i, f := range files {
			fds[i] = int(f.Fd())
		}
		oob = syscall.UnixRights(fds...)
	}
	_, _, err := s.conn.WriteMsgUnix(b[:], oob, nil)
	return err
}

func (s *forkServer) recv() (op, a1, a2 uint64, err error) {
	var b [32]byte
	n, err := io.ReadFull(s.conn, b[:])
	if err == io.ErrUnexpectedEOF {
		return 0, 0, 0, fmt.Errorf("short reply from fork server: %d bytes", n)
	}
	if err != nil {
		return 0, 0, 0, err
	}
	return binary.LittleEndian.Uint64(b[0:]), binary.LittleEndian.Uint64(b[8:]), binary.LittleEndian.Uint64(b[16:]), nil
}

// run lets the program run to switch n and stop there. If it ends first,
// run returns false; the program then runs to its end.
func (s *forkServer) run(n uint64) (bool, error) {
	ok, _, err := s.runFlags(n, 0)
	return ok, err
}

// runNovel lets the program run until the switch after it first records a
// feedback feature that the seen map it was forked with lacks, and returns
// that feature, or false if the program ended first.
func (s *forkServer) runNovel() (ok bool, feature int, err error) {
	ok, novel, err := s.runFlags(0, forkRunStopNovel)
	return ok, int(novel) - 1, err
}

func (s *forkServer) runFlags(n, flags uint64) (bool, uint64, error) {
	if err := s.send(forkRun, n, flags, 0); err != nil {
		return false, 0, err
	}
	op, a1, a2, err := s.recv()
	if errors.Is(err, io.EOF) {
		s.at = 0
		return false, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	if op != forkStopped {
		return false, 0, fmt.Errorf("fork server: unexpected reply %d", op)
	}
	s.at = a1
	return true, a2, nil
}

// branchFiles are the descriptors a forked child gets: its standard output
// and error, its own control socket if control, and optionally a file to
// write its feedback to at exit and the map of features seen so far.
type branchFiles struct {
	stdout, stderr *os.File
	control        bool
	feedback, seen *os.File
}

// branch forks a child that continues with the branch point (s.at, seed,
// steps), where steps, if not 0, is PCT's horizon (the tape's N:S:K),
// with the descriptors bf. With bf.control, the child is itself a fork
// server stopped at s.at; otherwise it runs to its end. It returns the
// child's PID.
func (s *forkServer) branch(seed, steps uint64, bf branchFiles) (int, *forkServer, error) {
	files := []*os.File{bf.stdout, bf.stderr}
	var child *forkServer
	var flags uint64
	if bf.control {
		conn, f, err := socketPair()
		if err != nil {
			return 0, nil, err
		}
		defer f.Close()
		child = &forkServer{conn: conn, at: s.at}
		files = append(files, f)
		flags |= forkWithControl
	}
	if bf.feedback != nil {
		files = append(files, bf.feedback)
		flags |= forkWithFeedback
	}
	if bf.seen != nil {
		files = append(files, bf.seen)
		flags |= forkWithSeen
	}
	if err := s.send(forkBranch, seed, steps, flags, files...); err != nil {
		if child != nil {
			child.conn.Close()
		}
		return 0, nil, err
	}
	op, a1, a2, err := s.recv()
	if err != nil {
		if child != nil {
			child.conn.Close()
		}
		return 0, nil, err
	}
	switch op {
	case forkPID:
	case forkRefused:
		if child != nil {
			child.conn.Close()
		}
		if a1 == 7 {
			return 0, nil, fmt.Errorf("fork server: clone failed: %v", syscall.Errno(a2))
		}
		return 0, nil, errRefused{a1}
	default:
		if child != nil {
			child.conn.Close()
		}
		return 0, nil, fmt.Errorf("fork server: unexpected reply %d", op)
	}
	if child != nil {
		if op, _, _, err := child.recv(); err != nil || op != forkReady {
			child.conn.Close()
			return 0, nil, fmt.Errorf("forked child did not report ready (%d, %v)", op, err)
		}
	}
	return int(a1), child, nil
}

// wait waits for child pid, forked by this server, to exit and returns its
// exit code, or -1 if a signal ended it.
func (s *forkServer) wait(pid int) (int, error) {
	if err := s.send(forkWait, uint64(pid), 0, 0); err != nil {
		return 0, err
	}
	op, a1, a2, err := s.recv()
	if err != nil {
		return 0, err
	}
	if op != forkStatus || a1 != uint64(pid) {
		return 0, fmt.Errorf("fork server: unexpected reply %d for wait %d", op, pid)
	}
	if a2&(1<<63) != 0 {
		return 0, fmt.Errorf("fork server: wait %d: %v", pid, syscall.Errno(a2&^(1<<63)))
	}
	ws := syscall.WaitStatus(a2)
	if ws.Signaled() {
		return -1, nil
	}
	return ws.ExitStatus(), nil
}

// close ends the program if it is still stopped and releases the server.
// A forked child's parent must then wait for it, with the PID that branch
// returned.
func (s *forkServer) close() {
	if s.at != 0 {
		s.send(forkExit, 0, 0, 0)
	}
	s.conn.Close()
	if s.cmd != nil {
		s.cmd.Wait()
	}
}
