// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simsharedfd shares a file and a connection between simulated
// nodes through package variables, as a harness that keeps a client in a
// global does, and keeps a file across a restart of its node. Each
// node's descriptors are its own, as a process's are: the other node's
// calls, and the new start's, must fail with EBADF, and the simulation
// must say once per World which node used whose descriptor in which call.
// Before, the calls worked, as all nodes shared one table. The owner's
// own uses must still work, from its goroutines, its OnStop function and
// a finalizer, and so must the test body's. TestSimPrograms runs it and
// checks the messages.
package simsharedfd

import (
	"errors"
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

var (
	shared     *os.File
	sharedConn net.Conn
)

// open opens a file that it drops, so that its finalizer closes it, on
// the node that opened it, and a descriptor that a cleanup closes there,
// sending the error to cleaned.
//
//go:noinline
func open(name string, cleaned chan error) error {
	if _, err := os.Create(name); err != nil {
		return err
	}
	fd, err := syscall.Open(name+".raw", syscall.O_CREAT|syscall.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	runtime.AddCleanup(new([16]byte), func(fd int) { cleaned <- syscall.Close(fd) }, fd)
	return nil
}

func TestSharedFile(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a, b := w.AddNode("a", "10.0.0.1"), w.AddNode("b", "10.0.0.2")
		cleaned := make(chan error, 1)
		<-a.Start(func() {
			f, err := os.Create("/shared")
			if err != nil {
				t.Error(err)
				return
			}
			shared = f
			if err := open("/dropped", cleaned); err != nil {
				t.Error(err)
			}
			runtime.GC()
			done := make(chan error)
			go func() {
				_, err := shared.Write([]byte("a"))
				done <- err
			}()
			if err := <-done; err != nil {
				t.Errorf("a goroutine of the node that opened the file: %v", err)
			}
		})
		runtime.GC() // the dropped file's finalizer and the cleanup run on a
		if err := <-cleaned; err != nil {
			t.Errorf("a cleanup set on the node that opened the descriptor: %v", err)
		}
		<-b.Start(func() {
			if _, err := shared.Write([]byte("b")); !errors.Is(err, syscall.EBADF) {
				t.Errorf("another node's write: %v, want EBADF", err)
			}
			if _, err := shared.Stat(); !errors.Is(err, syscall.EBADF) {
				t.Errorf("another node's fstat: %v, want EBADF", err)
			}
		})
		done := make(chan bool)
		a.Go(func() {
			if _, err := shared.Write([]byte("a")); err != nil {
				t.Errorf("the node that opened the file, after another node used it: %v", err)
			}
			done <- true
		})
		<-done
		if err := shared.Close(); err != nil {
			t.Errorf("the test body, of no node: %v", err)
		}
	})
}

func TestSharedConn(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		server, client, other := w.AddNode("server", "10.0.0.1"), w.AddNode("client", "10.0.0.2"), w.AddNode("other", "10.0.0.3")
		got := make(chan string, 1)
		server.Start(func() {
			l, err := net.Listen("tcp", ":7000")
			if err != nil {
				t.Error(err)
				return
			}
			c, err := l.Accept()
			if err != nil {
				t.Error(err)
				return
			}
			b, _ := io.ReadAll(c)
			got <- string(b)
		})
		<-client.Start(func() {
			c, err := net.Dial("tcp", "10.0.0.1:7000")
			if err != nil {
				t.Error(err)
				return
			}
			sharedConn = c
		})
		<-other.Start(func() {
			if _, err := sharedConn.Write([]byte("other")); !errors.Is(err, syscall.EBADF) {
				t.Errorf("another node's write: %v, want EBADF", err)
			}
		})
		client.Go(func() {
			if _, err := sharedConn.Write([]byte("client")); err != nil {
				t.Errorf("the node that dialed: %v", err)
			}
			sharedConn.Close()
		})
		if s := <-got; s != "client" {
			t.Errorf("the server read %q, want only the client's write", s)
		}
	})
}

func TestSharedRestart(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		r := w.AddNode("r", "10.0.0.1")
		<-r.Start(func() {
			f, err := os.Create("/kept")
			if err != nil {
				t.Error(err)
				return
			}
			shared = f
			closed, err := os.Create("/closed")
			if err != nil {
				t.Error(err)
				return
			}
			r.OnStop(func() {
				if err := closed.Close(); err != nil {
					t.Errorf("the OnStop function of the node that opened the file: %v", err)
				}
			})
		})
		r.Stop(time.Second) // closes /kept, which it left open
		<-r.Start(func() {
			if _, err := shared.Write([]byte("again")); !errors.Is(err, syscall.EBADF) {
				t.Errorf("a new start's write to the last start's file: %v, want EBADF", err)
			}
		})
	})
}
