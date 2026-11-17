// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simclients runs package detsched/sdk/compose with three client
// nodes and reports whether each ran commands and whether a partition put
// two of them on different sides, one reaching the server and one not.
package simclients

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/compose"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

const addr = "10.0.0.1:7000"

// serve accepts connections and closes them.
func serve() {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return
	}
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		c.Close()
	}
}

// TestClients runs timelines, each in a new world, until it has seen all
// three clients run commands and a partition with two of them on
// different sides, up to 10: each timeline draws its faults, and some
// have no partition.
func TestClients(t *testing.T) {
	if !sim.Supported() {
		t.Skip("requires GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var ran map[*sim.Node]bool
		most, split := 0, false
		for i := 0; i < 10 && !(most == 3 && split); i++ {
			w := sim.NewWorld(sim.Config{})
			server := w.AddNode("server", "10.0.0.1")
			server.Start(serve)
			ran = map[*sim.Node]bool{}
			c := compose.New(w, compose.Config{
				Clients: 3,
				Faults: sim.NemesisConfig{
					Restart: func(n *sim.Node) { n.Start(serve) },
					Event: func(ev sim.Event) {
						if !ev.Start || ev.Kind != sim.FaultPartition {
							return
						}
						mu.Lock()
						defer mu.Unlock()
						reach := map[bool]bool{}
						for n := range ran {
							reach[!ev.Net.Covers(n, server) && !ev.Net.Covers(server, n)] = true
						}
						split = split || reach[true] && reach[false]
					},
				},
				Budget: 20 * time.Second,
			})
			c.Parallel("dial", func(x *compose.Ctx) error {
				mu.Lock()
				ran[x.Node()] = true
				mu.Unlock()
				if conn, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
					conn.Close()
				}
				return nil
			})
			c.Run(t)
			w.Close()
			if sim.BranchNumber() != 0 {
				return // a branch of the timeline, which reports nothing
			}
			most = max(most, len(ran))
		}
		fmt.Printf("clients that ran commands: %d, two on different sides of a partition: %v\n", most, split)
	})
}
