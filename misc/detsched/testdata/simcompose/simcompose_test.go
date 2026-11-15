// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simcompose runs a tiny durable key-value server on a simulated
// node under package detsched/sdk/compose: parallel drivers store keys,
// a serial driver compacts the server's log, an anytime check reads
// back acknowledged keys while faults run, and the terminal checks are
// that the server takes writes again once faults stop (eventually) and
// that every acknowledged key survived (finally, with faults off or on).
//
// SIMCOMPOSE_BUG=1 makes the server acknowledge a put before it syncs
// it, syncing every 50ms instead, so that a power failure loses puts it
// acknowledged.
package simcompose

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/buggify"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/compose"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

const addr = "10.0.0.1:7000"

var bug = os.Getenv("SIMCOMPOSE_BUG") == "1"

// serve recovers the server's keys from /log and serves "put k v" and
// "get k" over TCP, one request per connection.
func serve() {
	f, err := os.OpenFile("/log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	var mu sync.Mutex
	kv := map[string]string{}
	data, _ := os.ReadFile("/log")
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[k] = v
		}
	}
	if d, err := os.Open("/"); err == nil {
		d.Sync()
		d.Close()
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return
	}
	if bug {
		// Sync in the background instead, for throughput.
		go func() {
			for {
				time.Sleep(50 * time.Millisecond)
				mu.Lock()
				f.Sync()
				mu.Unlock()
			}
		}()
	}
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))
			line, err := bufio.NewReader(c).ReadString('\n')
			if err != nil {
				return
			}
			f1 := strings.Fields(line)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case len(f1) == 3 && f1[0] == "put":
				if _, err := fmt.Fprintf(f, "%s=%s\n", f1[1], f1[2]); err != nil {
					return
				}
				if !bug && f.Sync() != nil {
					return
				}
				kv[f1[1]] = f1[2]
				fmt.Fprintf(c, "ok\n")
			case len(f1) == 1 && f1[0] == "compact":
				// Rewrite the log with one line per key, and replace
				// the old one atomically.
				t, err := os.Create("/log.tmp")
				if err != nil {
					return
				}
				for k, v := range kv {
					if _, err := fmt.Fprintf(t, "%s=%s\n", k, v); err != nil {
						t.Close()
						return
					}
				}
				if t.Sync() != nil || t.Close() != nil || os.Rename("/log.tmp", "/log") != nil {
					return
				}
				if d, err := os.Open("/"); err == nil {
					d.Sync()
					d.Close()
				}
				f.Close()
				if f, err = os.OpenFile("/log", os.O_RDWR|os.O_APPEND, 0o644); err != nil {
					return
				}
				fmt.Fprintf(c, "ok\n")
			case len(f1) == 2 && f1[0] == "get":
				v, ok := kv[f1[1]]
				if !ok {
					v = "missing"
				}
				fmt.Fprintf(c, "%s\n", v)
			}
		}()
	}
}

// call sends one request and returns the reply's first line.
func call(req string) (string, error) {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprintf(c, "%s\n", req); err != nil {
		return "", err
	}
	reply, err := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(reply), err
}

func TestCompose(t *testing.T) {
	if !sim.Supported() {
		t.Skip("requires GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		// The server does not survive disk errors: it ignores a failed
		// read of its log, and a failed append can leave half a line.
		buggify.Disable("sim.disk.fsyncfail", "sim.disk.enospc", "sim.disk.readerror")
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		server := w.AddNode("server", "10.0.0.1")
		server.Start(serve)

		// acked holds the keys the server acknowledged. Drivers add to
		// it; checks read it once the drivers have returned.
		var mu sync.Mutex
		acked := map[string]string{}
		next := 0

		c := compose.New(w, compose.Config{
			Faults: sim.NemesisConfig{
				Restart: func(n *sim.Node) { n.Start(serve) },
			},
			Budget: 40 * time.Second,
		})
		c.First("wait-for-server", func(x *compose.Ctx) error {
			return x.Until(10*time.Second, func() error {
				_, err := call("get none")
				return err
			})
		})
		c.Parallel("put", func(x *compose.Ctx) error {
			for range 1 + sim.Intn(4) {
				mu.Lock()
				next++
				k, v := fmt.Sprintf("k%d", next), fmt.Sprintf("v%d", sim.Intn(1000))
				mu.Unlock()
				if r, err := call("put " + k + " " + v); err == nil && r == "ok" {
					mu.Lock()
					acked[k] = v
					mu.Unlock()
				}
			}
			return nil
		})
		c.Serial("compact", func(x *compose.Ctx) error {
			call("compact")
			return nil
		})
		// readAcked checks that every acknowledged key the server reports
		// has its value; with all, also that none is missing.
		readAcked := func(all bool) error {
			mu.Lock()
			keys := make(map[string]string, len(acked))
			for k, v := range acked {
				keys[k] = v
			}
			mu.Unlock()
			for k, v := range keys {
				got, err := call("get " + k)
				if err != nil {
					if all {
						return fmt.Errorf("get %s: %v", k, err)
					}
					continue // a fault: the server is unreachable
				}
				if got != v {
					return fmt.Errorf("get %s = %s, want %s, as acknowledged", k, got, v)
				}
			}
			return nil
		}
		c.Anytime("read-acked", func(x *compose.Ctx) error { return readAcked(false) })
		c.Eventually("takes-writes", func(x *compose.Ctx) error {
			return x.Until(30*time.Second, func() error {
				r, err := call("put after recovery")
				if err == nil && r != "ok" {
					err = fmt.Errorf("put: %q", r)
				}
				return err
			})
		})
		c.Finally("acked-present", func(x *compose.Ctx) error {
			if err := x.Until(30*time.Second, func() error { _, err := call("get none"); return err }); err != nil {
				return err
			}
			return readAcked(true)
		})
		c.Finally("acked-never-wrong", func(x *compose.Ctx) error {
			return readAcked(false)
		}, compose.FaultsOn)
		c.Run(t)
		fmt.Printf("%d keys acknowledged\n", len(acked))
	})
}
