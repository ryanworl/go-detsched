// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simkill kills a throttled node while its goroutine reads a file
// it mapped. A killed node's goroutines must never run once its memory is
// unmapped, under any schedule: TestSimKill explores schedules.
package simkill

import (
	"io"
	"net"
	"os"
	"runtime"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

var sink byte

func TestKillThrottled(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		for i := range 20 {
			a.Start(func() {})
			mapped := make(chan bool)
			a.Go(func() {
				os.WriteFile("/f", make([]byte, 4096), 0o644)
				f, _ := os.Open("/f")
				m, err := syscall.Mmap(int(f.Fd()), 0, 4096, syscall.PROT_READ, syscall.MAP_SHARED)
				close(mapped)
				if err != nil {
					return
				}
				// Wake at the same instants as the goroutine killing the
				// node, so that it may run while the node is killed.
				for {
					time.Sleep(time.Millisecond)
					sink += m[0]
				}
			})
			<-mapped
			a.Throttle(2)
			time.Sleep(time.Duration(i+1) * 3 * time.Millisecond)
			a.Kill()
		}
	})
}

// TestKillStarting kills a node while its goroutine keeps starting
// goroutines on a paused node. The killed node's goroutine must not run
// again once the paused node resumes.
func TestKillStarting(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a, b := w.AddNode("a", "10.0.0.1"), w.AddNode("b", "10.0.0.2")
		b.Start(func() {})
		for range 20 {
			a.Start(func() {})
			b.Pause()
			n := 0
			a.Go(func() {
				for {
					b.Go(func() {})
					n++
				}
			})
			for range 100 {
				runtime.Gosched()
			}
			a.Kill()
			b.Resume()
			killed := n
			for range 100 {
				runtime.Gosched()
			}
			if n != killed {
				t.Fatalf("node a's goroutine started %d goroutines after a.Kill", n-killed)
			}
		}
	})
}

// TestStopTwice stops a node from two goroutines at once. The second Stop
// must not run the OnStop functions again: a program's shutdown, such as
// a Close that panics when called twice, runs once per process.
func TestStopTwice(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		a.Start(func() {})
		stops := 0
		a.OnStop(func() {
			stops++
			time.Sleep(time.Second)
		})
		done := make(chan bool)
		go func() {
			a.Stop(5 * time.Second)
			done <- true
		}()
		time.Sleep(100 * time.Millisecond)
		a.Stop(5 * time.Second)
		<-done
		if stops != 1 {
			t.Fatalf("two concurrent Stops ran the OnStop function %d times, want 1", stops)
		}
		if a.Up() {
			t.Fatalf("node a is up after Stop")
		}
	})
}

// TestSetIP moves a node that is down to a new address: dials to the new
// address reach it once it restarts, and dials to the old one reach
// nothing.
func TestSetIP(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a, b := w.AddNode("a", "10.0.0.1"), w.AddNode("b", "10.0.0.2")
		serve := func() {
			l, err := net.Listen("tcp", ":7000")
			if err != nil {
				panic(err)
			}
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Write([]byte(a.IP()))
				c.Close()
			}
		}
		a.Start(serve)
		b.Start(func() {})
		dial := func(addr string) (string, error) {
			done := make(chan error)
			var got []byte
			b.Go(func() {
				c, err := net.DialTimeout("tcp", addr, time.Second)
				if err == nil {
					got, err = io.ReadAll(c)
					c.Close()
				}
				done <- err
			})
			err := <-done
			return string(got), err
		}
		if got, err := dial("10.0.0.1:7000"); err != nil || got != "10.0.0.1" {
			t.Fatalf("dial before the move: %q, %v", got, err)
		}
		a.Kill()
		a.SetIP("10.0.1.7")
		a.Start(serve)
		if got, err := dial("10.0.1.7:7000"); err != nil || got != "10.0.1.7" {
			t.Errorf("dial to the new address: %q, %v", got, err)
		}
		if got, err := dial("10.0.0.1:7000"); err == nil {
			t.Errorf("dial to the old address reached %q", got)
		}
	})
}
