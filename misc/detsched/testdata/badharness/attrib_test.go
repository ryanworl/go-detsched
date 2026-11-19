// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package badharness

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/assert"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/buggify"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

// TestSlowServer fails when the nemesis pauses the server for longer than
// the client's deadline. Clock skips, which move the wall clock and not
// the deadline's, come in the same storms, so they fire in the failing
// runs far more often than in the passing ones: correlation names them
// too, and the summary must say that the failure needs pauses and not
// clock skips.
func TestSlowServer(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a, b := w.AddNode("a", "10.0.0.1"), w.AddNode("b", "10.0.0.2")
		a.Start(func() {
			l, err := net.Listen("tcp", ":7")
			if err != nil {
				return
			}
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				go func() {
					r := bufio.NewReader(c)
					for {
						line, err := r.ReadString('\n')
						if err != nil {
							return
						}
						c.Write([]byte(line))
					}
				}()
			}
		})
		b.Start(func() {})
		nm := w.NewNemesis(sim.NemesisConfig{
			Nodes:   []*sim.Node{a},
			Disable: []string{sim.FaultPartition, sim.FaultClog, sim.FaultLink, sim.FaultRestore, sim.FaultThrottle, sim.FaultDisk},
		})
		nm.Start()
		done := make(chan bool)
		b.Go(func() {
			defer close(done)
			var c net.Conn
			for range 300 {
				var err error
				if c == nil {
					c, err = net.Dial("tcp", "10.0.0.1:7")
				}
				if err == nil {
					c.SetDeadline(time.Now().Add(2 * time.Second))
					if _, err = c.Write([]byte("x\n")); err == nil {
						_, err = bufio.NewReader(c).ReadString('\n')
					}
				}
				assert.Always(!errors.Is(err, os.ErrDeadlineExceeded), "a request gets its response within 2s", map[string]any{"err": err})
				if err != nil && c != nil {
					c.Close()
					c = nil
				}
				time.Sleep(200 * time.Millisecond)
			}
		})
		<-done
		nm.Stop()
	})
}

// TestLostUpdate fails long after its cause, as a liveness bug does: the
// receiver opens the sender's window with one datagram per ten messages
// and never sends it again, so a lost one stalls the transfer for good,
// which the test notices only at its deadline. Reseeded close to that
// deadline, every run fails whatever faults are on, so only variants
// that differ from the start show that the failure needs the loss.
func TestLostUpdate(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		buggify.Disable("sim.net.udpdrop")
		a, b := w.AddNode("a", "10.0.0.1"), w.AddNode("b", "10.0.0.2")
		w.SetLink(b, a, sim.Link{Latency: time.Millisecond, Jitter: time.Millisecond, Drop: 0.01})
		const total, step = 200, 10
		b.Start(func() {
			l, err := net.Listen("tcp", ":7")
			if err != nil {
				return
			}
			c, err := l.Accept()
			if err != nil {
				return
			}
			u, err := net.Dial("udp", "10.0.0.1:8")
			if err != nil {
				return
			}
			r := bufio.NewReader(c)
			for n := 1; ; n++ {
				if _, err := r.ReadString('\n'); err != nil {
					return
				}
				if n%step == 0 {
					fmt.Fprintf(u, "%d", n+step) // the new window, sent once
				}
			}
		})
		done := make(chan bool)
		a.Start(func() {
			u, err := net.ListenPacket("udp", ":8")
			if err != nil {
				return
			}
			window := make(chan int, total)
			go func() {
				buf := make([]byte, 16)
				for {
					n, _, err := u.ReadFrom(buf)
					if err != nil {
						return
					}
					w, _ := strconv.Atoi(string(buf[:n]))
					window <- w
				}
			}()
			c, err := net.Dial("tcp", "10.0.0.2:7")
			if err != nil {
				return
			}
			limit := step
			for sent := 0; sent < total; sent++ {
				for sent >= limit {
					limit = max(limit, <-window)
				}
				c.Write([]byte("chunk\n"))
				time.Sleep(100 * time.Millisecond)
			}
			close(done)
		})
		select {
		case <-done:
		case <-time.After(60 * time.Second):
		}
		finished := false
		select {
		case <-done:
			finished = true
		default:
		}
		assert.Always(finished, "the transfer finishes within 60s", nil)
	})
}
