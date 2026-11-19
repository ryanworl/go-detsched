// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Ptrmap ranges over maps keyed by pointers: it closes channels in the
// order a map keyed by them gives, as a set of watch channels does, while
// a goroutine waits on each, and it prints the order in which they woke
// and in which maps keyed by pointers, by structs holding them, by
// interfaces holding them, and a sync.Map visit their keys. Between the
// keys it allocates objects of the keys' sizes that the optimized build
// keeps on the stack and the build without optimizations puts on the
// heap, so the keys' addresses differ between the builds.
package main

import (
	"fmt"
	"sync"
)

type node struct {
	id   int
	next *node
}

type pair struct {
	n *node
	k int
}

// small and large are as large as a node and a channel, so that they
// share their size classes.
type (
	small [2]*int
	large [12]*int
)

//go:noinline
func use(s *small, l *large) int { return len(s) + len(l) }

func scratch() (*small, *large) { return new(small), new(large) }

func main() {
	const n = 48
	chans := map[chan struct{}]int{}
	nodes := map[*node]int{}
	pairs := map[pair]int{}
	ifaces := map[any]int{}
	var sm sync.Map
	woke := make(chan int)
	sum := 0
	var prev *node
	for i := range n {
		sum += use(scratch())
		ch := make(chan struct{})
		chans[ch] = i
		go func() {
			<-ch
			woke <- i
		}()
		nd := &node{id: i, next: prev}
		prev = nd
		nodes[nd] = i
		pairs[pair{nd, i % 3}] = i
		ifaces[nd] = i
		sm.Store(nd, i)
	}
	for ch := range chans {
		close(ch)
	}
	var order []int
	for range n {
		order = append(order, <-woke)
	}
	fmt.Println("woke:", order)
	var ids []int
	for nd := range nodes {
		ids = append(ids, nd.id)
	}
	fmt.Println("nodes:", ids)
	ids = ids[:0]
	for p := range pairs {
		ids = append(ids, p.n.id)
	}
	fmt.Println("pairs:", ids)
	ids = ids[:0]
	for k := range ifaces {
		ids = append(ids, k.(*node).id)
	}
	fmt.Println("interfaces:", ids)
	ids = ids[:0]
	sm.Range(func(k, v any) bool {
		ids = append(ids, k.(*node).id)
		return true
	})
	fmt.Println("sync.Map:", ids, sum)
}
