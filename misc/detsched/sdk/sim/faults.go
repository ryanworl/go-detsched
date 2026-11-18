// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build (linux && (amd64 || arm64)) || (darwin && !ios && arm64)

package sim

import (
	"math"
	"slices"
	"time"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/assert"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/buggify"
)

// Network faults, after Antithesis's catalog: each directed link between
// two nodes has a baseline (Link) and is affected by the active faults
// (NetFault) that cover it. A fault acts on every message, including on
// connections that already exist, in one of three modes.

// Mode is how a network fault treats the traffic it covers.
type Mode int

const (
	// Stopped drops packets: TCP holds data until the fault ends, and a
	// connection whose data waits 30 seconds fails with ETIMEDOUT.
	// Connection attempts fail with ETIMEDOUT unless the fault ends
	// within 20 seconds.
	Stopped Mode = iota
	// Slowed adds latency, normally distributed with Effect.Mean and
	// Effect.Dev, to each message sent while the fault lasts.
	Slowed
	// Jammed queues packets and delivers them when the fault ends,
	// without timing out.
	Jammed
)

func (m Mode) String() string {
	switch m {
	case Stopped:
		return "stopped"
	case Slowed:
		return "slowed"
	case Jammed:
		return "jammed"
	}
	return "mode?"
}

// Effect is what a network fault does to the traffic it covers.
type Effect struct {
	Mode      Mode
	Mean, Dev time.Duration // added latency, for Slowed
}

// Link is the baseline behavior of the traffic from one node to another.
type Link struct {
	// Latency is the base one-way latency.
	Latency time.Duration
	// Jitter is the mean of an exponentially distributed delay added to
	// each message.
	Jitter time.Duration
	// Drop is the probability that a packet is lost. Over TCP, a lost
	// packet shows up as a retransmission delay, from 200ms, doubling
	// for each further loss.
	Drop float64
	// MTU is the largest IP packet the path carries, 1500 bytes if 0. A
	// UDP datagram that would make a larger packet is lost.
	MTU int
}

// NetFault is an active network fault, from its creation until Heal or
// World.Heal.
type NetFault struct {
	w     *World
	id    int
	eff   Effect
	links [][2]int // directed links, by node index
	desc  string
}

// String describes the fault, such as "partition stopped [n1] | [n2 n3]".
func (f *NetFault) String() string { return f.desc }

// Effect returns what the fault does.
func (f *NetFault) Effect() Effect { return f.eff }

// Covers reports whether the fault affects traffic from one node to
// another.
func (f *NetFault) Covers(from, to *Node) bool {
	return slices.Contains(f.links, [2]int{from.index, to.index})
}

// SetLink sets the baseline of the traffic from one node to another. Links
// start with a latency of 1ms and a jitter of 1ms.
func (w *World) SetLink(from, to *Node, l Link) {
	runtime_call(func() { w.links[[2]int{from.index, to.index}] = l })
}

// SetNAT puts n behind a NAT, or takes it out of one: each local UDP port
// that sends to another node gets a public port, which the other nodes see
// as the datagrams' source, and only datagrams to a public port in use
// reach n. Rebind makes the NAT forget its ports, as one that restarts or
// times out its mappings does, so that n's traffic comes from new ports
// (NAT rebinding). TCP is not translated.
func (n *Node) SetNAT(on bool) {
	runtime_call(func() {
		switch {
		case on && n.nat == nil:
			n.nat, n.natRev, n.natNext = map[uint16]uint16{}, map[uint16]uint16{}, 20000
		case !on:
			n.nat, n.natRev = nil, nil
		}
	})
}

// Rebind gives each of n's local UDP ports a new public port, if n is
// behind a NAT (SetNAT); datagrams to the old ones are lost.
func (n *Node) Rebind() {
	runtime_call(func() {
		clear(n.nat)
		clear(n.natRev)
	})
}

// natPort returns the public port for local port p of n, which is behind
// a NAT.
func (n *Node) natPort(p uint16) uint16 {
	if q, ok := n.nat[p]; ok {
		return q
	}
	for {
		q := n.natNext
		n.natNext++
		if n.natNext >= 60000 {
			n.natNext = 20000
		}
		if _, used := n.natRev[q]; !used {
			n.nat[p], n.natRev[q] = q, p
			return q
		}
	}
}

// PartitionGroups splits the nodes into groups and applies eff to the
// traffic between nodes of different groups: in both directions, or, if
// oneWay, only from each group to the groups after it. Nodes in no group
// form a group of their own.
func (w *World) PartitionGroups(eff Effect, oneWay bool, groups ...[]*Node) *NetFault {
	group := make(map[*Node]int)
	for i, g := range groups {
		for _, n := range g {
			group[n] = i + 1
		}
	}
	rest := len(groups) + 1
	var links [][2]int
	desc := "partition " + eff.Mode.String()
	if oneWay {
		desc = "one-way " + desc
	}
	for i, g := range groups {
		if i > 0 {
			desc += " |"
		}
		desc += " " + names(g)
	}
	for _, a := range w.nodes {
		for _, b := range w.nodes {
			ga, gb := group[a], group[b]
			if ga == 0 {
				ga = rest
			}
			if gb == 0 {
				gb = rest
			}
			if ga != gb && (!oneWay || ga < gb) {
				links = append(links, [2]int{a.index, b.index})
			}
		}
	}
	return w.addFault(eff, links, desc)
}

// Clog applies eff to n's inbound traffic, outbound traffic, or both.
func (w *World) Clog(n *Node, in, out bool, eff Effect) *NetFault {
	var links [][2]int
	for _, m := range w.nodes {
		if m == n {
			continue
		}
		if in {
			links = append(links, [2]int{m.index, n.index})
		}
		if out {
			links = append(links, [2]int{n.index, m.index})
		}
	}
	dir := "in+out"
	if !in {
		dir = "out"
	} else if !out {
		dir = "in"
	}
	return w.addFault(eff, links, "clog "+eff.Mode.String()+" "+n.name+" "+dir)
}

// Cut applies eff to the traffic from one node to another.
func (w *World) Cut(from, to *Node, eff Effect) *NetFault {
	return w.addFault(eff, [][2]int{{from.index, to.index}}, "cut "+eff.Mode.String()+" "+from.name+"->"+to.name)
}

func (w *World) addFault(eff Effect, links [][2]int, desc string) *NetFault {
	f := &NetFault{w: w, eff: eff, links: links, desc: desc}
	runtime_call(func() {
		w.nextFault++
		f.id = w.nextFault
		w.faults = append(w.faults, f)
	})
	return f
}

// Heal ends the fault. Data it held is delivered.
func (f *NetFault) Heal() {
	w := f.w
	runtime_call(func() {
		if i := slices.Index(w.faults, f); i >= 0 {
			w.faults = slices.Delete(w.faults, i, i+1)
			w.netChanged()
		}
	})
}

// Faults returns the active network faults, oldest first.
func (w *World) Faults() []*NetFault {
	var fs []*NetFault
	runtime_call(func() { fs = slices.Clone(w.faults) })
	return fs
}

// Heal ends every network fault. Data held is delivered.
func (w *World) Heal() {
	runtime_call(func() {
		w.faults = nil
		w.netChanged()
	})
}

// Partition stops all traffic between the nodes in a and the nodes in b, in
// both directions.
func (w *World) Partition(a, b []*Node) *NetFault {
	links := make([][2]int, 0, 2*len(a)*len(b))
	for _, x := range a {
		for _, y := range b {
			links = append(links, [2]int{x.index, y.index}, [2]int{y.index, x.index})
		}
	}
	return w.addFault(Effect{}, links, "partition stopped "+names(a)+" | "+names(b))
}

// Isolate stops all traffic between n and every other node.
func (w *World) Isolate(n *Node) *NetFault { return w.Clog(n, true, true, Effect{}) }

// Block stops traffic from one node to another, in that direction only.
func (w *World) Block(from, to *Node) *NetFault { return w.Cut(from, to, Effect{}) }

func names(ns []*Node) string {
	s := "["
	for i, n := range ns {
		if i > 0 {
			s += " "
		}
		s += n.name
	}
	return s + "]"
}

// linkState is the effect of the active faults on the traffic from a to b.
type linkState struct {
	stopped, jammed bool
	slow            []Effect
}

func (w *World) linkState(a, b *Node) linkState {
	var st linkState
	if a == b {
		return st
	}
	k := [2]int{a.index, b.index}
	for _, f := range w.faults {
		if !slices.Contains(f.links, k) {
			continue
		}
		switch f.eff.Mode {
		case Stopped:
			st.stopped = true
		case Jammed:
			st.jammed = true
		case Slowed:
			st.slow = append(st.slow, f.eff)
		}
	}
	return st
}

// linkUp reports whether traffic from a to b gets through now.
func (w *World) linkUp(a, b *Node) bool {
	st := w.linkState(a, b)
	return !st.stopped && !st.jammed
}

// link returns the baseline of the traffic from a to b.
func (w *World) link(a, b *Node) Link {
	if l, ok := w.links[[2]int{a.index, b.index}]; ok && a != b {
		return l
	}
	if a == b {
		return Link{Latency: 50 * time.Microsecond}
	}
	return Link{Latency: latency, Jitter: jitter}
}

// normal returns a normally distributed duration with the given mean and
// standard deviation, not below 0.
func normal(mean, dev time.Duration) int64 {
	// Irwin-Hall: the sum of 12 uniform values, minus 6, is close to a
	// standard normal value.
	s := -6.0
	for range 12 {
		s += float64(runtime_rand()&(1<<26-1)) / (1 << 26)
	}
	return max(0, int64(mean)+int64(s*float64(dev)))
}

// fault reports whether to inject the fault at the buggify site with
// probability p, which calm keeps off, and reports each fault it injects
// as the assertion msg, "fault: " and the site, by which the detsched
// command's summary says in what share of each group's runs it fired.
func fault(site, msg string, p float64) bool {
	if calm() || !buggify.OnChance(site, p) {
		return false
	}
	injected(msg)
	return true
}

// injected reports a fault the simulation injects, as fault does.
func injected(msg string) {
	runtime_call(func() { assert.Reachable(msg, nil) })
}

// chance reports true with probability p, from the fault stream.
func chance(p float64) bool {
	return float64(runtime_rand()&(1<<53-1))/(1<<53) < p
}

// expRand returns an exponentially distributed duration with the given
// mean, from 26 random bits of the fault stream.
func expRand(mean time.Duration) time.Duration {
	u := float64(runtime_rand()&(1<<26-1)+1) / (1 << 26)
	return time.Duration(-math.Log(u) * float64(mean))
}
