// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simnonode dials and listens from goroutines that belong to no
// simulated node, the test body and one it starts, while a World is
// active. Their calls must fail, rather than use the real network, and
// a node's calls must still work. TestSimPrograms checks the
// message the simulation prints.
package simnonode

import (
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"testing/synctest"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

func TestNoNode(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a, b := w.AddNode("a", "10.0.0.1"), w.AddNode("b", "10.0.0.2")
		a.Start(func() {
			l, err := net.Listen("tcp", ":7000")
			if err != nil {
				panic(err)
			}
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Write([]byte("hello"))
				c.Close()
			}
		})
		b.Start(func() {})
		dial := func() {
			done := make(chan error)
			b.Go(func() {
				c, err := net.Dial("tcp", "10.0.0.1:7000")
				if err == nil {
					var b []byte
					b, err = io.ReadAll(c)
					if err == nil && string(b) != "hello" {
						err = errors.New("read " + string(b))
					}
					c.Close()
				}
				done <- err
			})
			if err := <-done; err != nil {
				t.Errorf("dial from a node: %v", err)
			}
		}
		dial()

		if c, err := net.Dial("tcp", "10.0.0.1:7000"); !errors.Is(err, syscall.EPERM) {
			t.Errorf("dial from the test body: got %v, %v; want EPERM", c, err)
		}
		done := make(chan error)
		go func() {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err == nil {
				l.Close()
			}
			done <- err
		}()
		if err := <-done; !errors.Is(err, syscall.EPERM) {
			t.Errorf("listen from a goroutine the test body started: got %v, want EPERM", err)
		}
		dial()
	})
}
