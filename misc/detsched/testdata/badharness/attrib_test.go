// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package badharness

import (
	"bufio"
	"errors"
	"net"
	"os"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/assert"
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
