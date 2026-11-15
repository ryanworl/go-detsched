// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simkv runs a tiny key-value server, written against plain net
// and os, on simulated nodes (detsched/sdk/sim): a client stores keys over
// TCP, the server appends them to a log file and fsyncs some of them, and
// the test crashes, restarts, and partitions nodes, printing what the
// client and servers see.
package simkv

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/buggify"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

var start time.Time

func logf(format string, args ...any) {
	fmt.Printf("%8v  %s\n", time.Since(start), fmt.Sprintf(format, args...))
}

// serve runs the server: it recovers its state from /data/log, then
// serves "put k v" (fsynced), "lazy k v" (not fsynced), and "get k".
func serve(n *sim.Node, addr string) {
	name := n.Name()
	if err := os.MkdirAll("/data", 0o755); err != nil {
		logf("%s: mkdir: %v", name, err)
		return
	}
	f, err := os.OpenFile("/data/log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		logf("%s: open: %v", name, err)
		return
	}
	// Make the log's directory entry durable: POSIX promises that only
	// after fsync of the directory.
	for _, dir := range []string{"/data", "/"} {
		if d, err := os.Open(dir); err == nil {
			if err := d.Sync(); err != nil {
				logf("%s: fsync %s: %v", name, dir, err)
			}
			d.Close()
		}
	}
	data, _ := io.ReadAll(f)
	kv := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[k] = v
		}
	}
	logf("%s: recovered %d keys", name, len(kv))
	l, err := net.Listen("tcp", addr)
	if err != nil {
		logf("%s: listen: %v", name, err)
		return
	}
	n.OnStop(func() {
		logf("%s: shutting down", name)
		l.Close()
	})
	stopped := make(chan struct{})
	defer close(stopped)
	for {
		c, err := l.Accept()
		if err != nil {
			logf("%s: accept: %v", name, err)
			return
		}
		go func() {
			defer c.Close()
			go func() { // a connection's reader stops when the server does
				<-stopped
				c.Close()
			}()
			r := bufio.NewScanner(c)
			for r.Scan() {
				op, rest, _ := strings.Cut(r.Text(), " ")
				k, v, _ := strings.Cut(rest, " ")
				switch op {
				case "put", "lazy":
					fmt.Fprintf(f, "%s=%s\n", k, v)
					if op == "put" {
						if err := f.Sync(); err != nil {
							// The write may or may not be durable.
							fmt.Fprintf(c, "error: %v\n", err)
							continue
						}
					}
					kv[k] = v
					fmt.Fprintf(c, "ok\n")
				case "get":
					fmt.Fprintf(c, "%s\n", kv[k])
				}
			}
		}()
	}
}

// call sends one request on c and returns the reply.
func call(c net.Conn, req string) (string, error) {
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := fmt.Fprintf(c, "%s\n", req); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(line), err
}

func dialRetry(addr string) net.Conn {
	for range 100 {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			return c
		}
		time.Sleep(100 * time.Millisecond)
	}
	panic("simkv: cannot reach " + addr)
}

func describe(err error) string {
	var ne net.Error
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout: " + err.Error()
	}
	return err.Error()
}

func TestSim(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	// The server stops on a disk error, so leave out the disk faults that
	// would keep it from starting.
	buggify.Disable("sim.disk.enospc", "sim.disk.readerror")
	synctest.Test(t, func(t *testing.T) {
		start = time.Now()
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		s1 := w.AddNode("s1", "10.0.0.1")
		s2 := w.AddNode("s2", "10.0.0.2")
		cl := w.AddNode("client", "10.0.0.9")
		s1.Start(func() { serve(s1, "10.0.0.1:7000") })
		s2.Start(func() { serve(s2, "10.0.0.2:7000") })

		steps := make(chan func(), 1)
		done := make(chan bool)
		cl.Start(func() {
			for f := range steps {
				f()
				done <- true
			}
		})
		do := func(f func()) { steps <- f; <-done }

		var c1, c2 net.Conn
		do(func() {
			// The server may not listen yet.
			c1 = dialRetry("10.0.0.1:7000")
			logf("client: dial s1: local %v", c1.LocalAddr())
			for i := range 3 {
				reply, err := call(c1, fmt.Sprintf("put a%d %d", i, i))
				logf("client: put a%d: %q %s", i, reply, describe(err))
			}
			reply, err := call(c1, "lazy b 1")
			logf("client: lazy b: %q %s", reply, describe(err))
			_, err = net.DialTimeout("tcp", "10.0.0.1:7001", time.Second)
			logf("client: dial s1:7001: %s", describe(err))
		})

		logf("kill s1 (power failure)")
		s1.Kill()
		do(func() {
			reply, err := call(c1, "get a0")
			logf("client: get a0 from killed s1: %q %s", reply, describe(err))
			_, err = net.DialTimeout("tcp", "10.0.0.1:7000", 3*time.Second)
			logf("client: dial killed s1: %s", describe(err))
		})
		logf("restart s1")
		s1.Start(func() { serve(s1, "10.0.0.1:7000") })
		time.Sleep(time.Second)
		do(func() {
			reply, err := call(c1, "get a0")
			logf("client: old connection to restarted s1: %q %s", reply, describe(err))
			c1 = dialRetry("10.0.0.1:7000")
			logf("client: redial s1: ok")
			for _, k := range []string{"a0", "a2", "b"} {
				reply, err := call(c1, "get "+k)
				logf("client: get %s: %q %s", k, reply, describe(err))
			}
		})

		do(func() {
			c2 = dialRetry("10.0.0.2:7000")
			logf("client: dial s2: ok")
		})
		logf("partition client from s2")
		w.Partition([]*sim.Node{cl}, []*sim.Node{s2})
		do(func() {
			reply, err := call(c2, "put c 1")
			logf("client: put c across partition: %q %s", reply, describe(err))
		})
		logf("heal")
		w.Heal()
		do(func() {
			// The request held during the partition arrives now, and so
			// does its reply, before the reply to this one.
			r := bufio.NewReader(c2)
			c2.SetDeadline(time.Now().Add(2 * time.Second))
			fmt.Fprintf(c2, "get c\n")
			for range 2 {
				line, err := r.ReadString('\n')
				logf("client: after heal: %q %s", strings.TrimSpace(line), describe(err))
			}
		})

		logf("kill the process of s2")
		s2.KillProcess()
		do(func() {
			reply, err := call(c2, "get c")
			logf("client: get c from s2 after its process died: %q %s", reply, describe(err))
		})
		logf("restart s2")
		s2.Start(func() { serve(s2, "10.0.0.2:7000") })
		do(func() {
			c2 = dialRetry("10.0.0.2:7000")
			reply, err := call(c2, "get c")
			logf("client: get c from restarted s2: %q %s", reply, describe(err))
		})

		logf("pause s2")
		s2.Pause()
		do(func() {
			reply, err := call(c2, "put d 1")
			logf("client: put d to paused s2: %q %s", reply, describe(err))
		})
		logf("resume s2")
		s2.Resume()
		do(func() {
			// The paused server buffered the request and answers it now.
			c2.SetDeadline(time.Now().Add(2 * time.Second))
			line, err := bufio.NewReader(c2).ReadString('\n')
			logf("client: after resume: %q %s", strings.TrimSpace(line), describe(err))
		})

		logf("stop s1")
		s1.Stop(5 * time.Second)
		logf("s1 stopped, up=%v", s1.Up())
		do(func() {
			reply, err := call(c1, "get a0")
			logf("client: get a0 from stopped s1: %q %s", reply, describe(err))
			_, err = net.DialTimeout("tcp", "10.0.0.1:7000", time.Second)
			logf("client: dial stopped s1: %s", describe(err))
		})
		logf("remove s1, add s3")
		w.RemoveNode(s1)
		s3 := w.AddNode("s3", "10.0.0.3")
		s3.Start(func() { serve(s3, "10.0.0.3:7000") })
		do(func() {
			c3 := dialRetry("10.0.0.3:7000")
			reply, err := call(c3, "put e 1")
			logf("client: put e to new s3: %q %s", reply, describe(err))
			_, err = net.DialTimeout("tcp", "10.0.0.1:7000", time.Second)
			logf("client: dial removed s1: %s", describe(err))
		})
		logf("add s1 back with its disk")
		w.ReAddNode(s1)
		s1.Start(func() { serve(s1, "10.0.0.1:7000") })
		do(func() {
			c1 = dialRetry("10.0.0.1:7000")
			reply, err := call(c1, "get a0")
			logf("client: get a0 from s1 back: %q %s", reply, describe(err))
		})
		logf("remove s3, wipe its disk, and add it back")
		w.RemoveNode(s3)
		s3.WipeDisk()
		w.ReAddNode(s3)
		s3.Start(func() { serve(s3, "10.0.0.3:7000") })
		do(func() {
			c3 := dialRetry("10.0.0.3:7000")
			reply, err := call(c3, "get e")
			logf("client: get e from wiped s3: %q %s", reply, describe(err))
		})
		close(steps)
	})
}

// TestFileLocks checks that flock and Linux's open file description locks
// belong to the open file: two opens of a file on one node conflict, and
// closing the file, or crashing its node, releases the lock.
func TestFileLocks(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		n := w.AddNode("n", "10.0.0.1")
		ofd := func(f *os.File, cmd int, typ int16) error {
			return syscall.FcntlFlock(f.Fd(), cmd, &syscall.Flock_t{Type: typ, Whence: io.SeekStart})
		}
		run := func(f func()) {
			done := make(chan bool)
			n.Go(func() {
				defer close(done)
				f()
			})
			<-done
		}
		n.Start(func() {})
		run(func() {
			a, err := os.Create("/lock")
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.OpenFile("/lock", os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Flock(int(a.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatalf("flock: %v", err)
			}
			if err := syscall.Flock(int(b.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
				t.Errorf("flock of a second open file of a locked file: %v, want EWOULDBLOCK", err)
			}
			if runtime.GOOS == "linux" {
				if err := ofd(a, fOFDSetlk, syscall.F_WRLCK); err != nil {
					t.Errorf("open file description lock of a file the same open file flocked: %v", err)
				}
				if err := ofd(b, fOFDSetlk, syscall.F_WRLCK); !errors.Is(err, syscall.EAGAIN) {
					t.Errorf("open file description lock of a locked file: %v, want EAGAIN", err)
				}
			}
			a.Close()
			if err := syscall.Flock(int(b.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Errorf("flock after the holder closed the file: %v", err)
			}
		})
		n.KillProcess()
		n.Start(func() {})
		run(func() {
			c, err := os.OpenFile("/lock", os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Flock(int(c.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Errorf("flock after the holder's process crashed: %v", err)
			}
		})
	})
}

// fOFDSetlk is Linux's F_OFD_SETLK, which package syscall lacks.
const fOFDSetlk = 37

// TestFailAtEnd fails, after the other tests ran, if SIMKV_FAIL is set,
// so that the run prints the fault timeline.
func TestFailAtEnd(t *testing.T) {
	if os.Getenv("SIMKV_FAIL") != "" {
		t.Error("SIMKV_FAIL is set")
	}
}
