// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simzombie shares a mutex between two simulated nodes, as
// libraries with global state do: node a crashes at a time the seed draws
// while it takes and releases the mutex in a loop, perhaps in the middle
// of Lock or Unlock, and node b must
// still get the mutex, from a zombie of a that runs until it releases it.
// With SIMZOMBIE_GC=1, a allocates in the critical section, so that the
// zombie may start a GC, which sets its bubble aside meanwhile. With
// SIMZOMBIE_RW=1, the mutex is a sync.RWMutex that a writes, sleeping
// while it holds it, and b reads.
package simzombie

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

var (
	shared sync.Mutex
	rw     sync.RWMutex
	// TestZombie can crash a after it locked rw.w but before it made
	// readers wait, so b reads rw and the frozen a keeps rw.w after its
	// bubble ended. The tests after it lock their own.
	rwWaiter, rwReader sync.RWMutex
	woken              sync.Mutex
)

func TestZombie(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		b := w.AddNode("b", "10.0.0.2")
		n := 0
		gc := os.Getenv("SIMZOMBIE_GC") == "1"
		if gc {
			defer debug.SetGCPercent(debug.SetGCPercent(1))
		}
		var keep [][]byte
		rwMode := os.Getenv("SIMZOMBIE_RW") == "1"
		a.Start(func() {
			for rwMode {
				rw.Lock()
				n++
				time.Sleep(10 * time.Microsecond)
				rw.Unlock()
				time.Sleep(time.Microsecond)
			}
			for {
				shared.Lock()
				n++
				if gc {
					for range 16 {
						keep = append(keep[len(keep)/2:], make([]byte, 64<<10))
					}
				}
				shared.Unlock()
				time.Sleep(time.Microsecond)
			}
		})
		time.Sleep(time.Duration(1+sim.Intn(1000)) * time.Microsecond)
		a.Kill()
		done := make(chan bool)
		b.Start(func() {
			if rwMode {
				rw.RLock()
				fmt.Println("b took the read lock")
				rw.RUnlock()
				done <- true
				return
			}
			shared.Lock()
			fmt.Println("b took the lock")
			shared.Unlock()
			done <- true
		})
		<-done
		// The clock advances only once every goroutine of the bubble
		// is blocked, which a bubble that miscounted a zombie never sees.
		time.Sleep(time.Second)
	})
}

// TestWaiter has an Unlock wake a goroutine of node a that waits for the
// mutex while a goroutine of node b waits too, and crashes a: before or
// after its waiter takes the mutex, as the schedule has it. b, which
// waits already, must still get the mutex, from a zombie of a.
func TestWaiter(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		b := w.AddNode("b", "10.0.0.2")
		shared.Lock()
		a.Start(func() {
			shared.Lock()
			shared.Unlock()
		})
		time.Sleep(time.Millisecond)
		done := make(chan bool)
		b.Start(func() {
			shared.Lock()
			shared.Unlock()
			done <- true
		})
		time.Sleep(time.Millisecond)
		shared.Unlock()
		a.Kill()
		<-done
		fmt.Println("b took the lock after a's waiter crashed")
		time.Sleep(time.Second)
	})
}

// TestLoser has an Unlock wake a goroutine of node a that waits for the
// mutex, crashes a, and takes the mutex before a's waiter, which runs on
// as a zombie for it, can: the zombie loses the mutex and waits again.
// It must wait outside the bubble, as frozen goroutines do, so that the
// bubble can end while the mutex is held.
func TestLoser(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		shared.Lock()
		a.Start(func() {
			shared.Lock()
			shared.Unlock()
		})
		time.Sleep(time.Millisecond)
		shared.Unlock()
		a.Kill()
		shared.Lock()
		time.Sleep(time.Second)
	})
}

// TestWoken has an Unlock wake a goroutine of node a that waits for the
// mutex, and crashes a before that goroutine runs. Only a waits for the
// mutex, so the goroutine must stay frozen: run on, it would touch its
// dead process's memory. It keeps the mutex's turn to be woken, so a live
// goroutine that waits for the mutex later must thaw it and still get it.
func TestWoken(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		var ranDead atomic.Bool
		woken.Lock()
		a.Start(func() {
			woken.Lock()
			if !a.Up() {
				ranDead.Store(true)
			}
			woken.Unlock()
		})
		time.Sleep(time.Millisecond)
		woken.Unlock()
		a.Kill()
		time.Sleep(time.Second)
		if ranDead.Load() {
			t.Error("a goroutine of a crashed node ran on, though no live goroutine waited for its mutex")
		}
		woken.Lock()
		done := make(chan bool)
		go func() {
			woken.Lock()
			woken.Unlock()
			close(done)
		}()
		time.Sleep(time.Millisecond)
		woken.Unlock()
		<-done
		time.Sleep(time.Second)
	})
}

// TestOwnWaiters crashes node a while one of its goroutines holds a
// read lock, another holds the write lock's mutex and waits for the
// reader, and a third waits for that mutex. No live goroutine waits, so
// the holder must stay frozen: thawed, it would wait in the bubble for a
// reader that never runs again.
func TestOwnWaiters(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		var rw sync.RWMutex
		a.Start(func() {
			rw.RLock()
			time.Sleep(time.Hour)
			rw.RUnlock()
		})
		time.Sleep(time.Millisecond)
		for range 2 {
			a.Go(func() {
				rw.Lock()
				rw.Unlock()
			})
			time.Sleep(time.Millisecond)
		}
		a.Kill()
		time.Sleep(time.Second)
	})
}

// TestRWWaiter crashes node a while it holds rwWaiter for writing and a
// goroutine of node b already waits to read it. b must still read it,
// from a zombie of a.
func TestRWWaiter(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		b := w.AddNode("b", "10.0.0.2")
		a.Start(func() {
			rwWaiter.Lock()
			time.Sleep(time.Second)
			rwWaiter.Unlock()
		})
		time.Sleep(time.Millisecond)
		done := make(chan bool)
		b.Start(func() {
			rwWaiter.RLock()
			rwWaiter.RUnlock()
			done <- true
		})
		time.Sleep(time.Millisecond)
		a.Kill()
		<-done
		fmt.Println("b read rw after its writer crashed")
		time.Sleep(time.Second)
	})
}

// TestRWReader crashes node a while it holds rwReader for reading, and node b
// then writes it: before the crash with SIMZOMBIE_RW=1, after it
// otherwise. b must still write it, from a zombie of a.
func TestRWReader(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		b := w.AddNode("b", "10.0.0.2")
		a.Start(func() {
			rwReader.RLock()
			time.Sleep(time.Second)
			rwReader.RUnlock()
		})
		time.Sleep(time.Millisecond)
		done := make(chan bool)
		write := func() {
			b.Start(func() {
				rwReader.Lock()
				rwReader.Unlock()
				done <- true
			})
		}
		early := os.Getenv("SIMZOMBIE_RW") == "1"
		if early {
			write()
			time.Sleep(time.Millisecond)
		}
		a.Kill()
		if !early {
			write()
		}
		<-done
		fmt.Println("b wrote rw after its reader crashed")
		time.Sleep(time.Second)
	})
}

// lockBox is a heap mutex whose memory TestStaleHolder reuses.
type lockBox struct {
	mu  sync.Mutex
	pad [1000]byte
}

// TestStaleHolder has a goroutine of node a lock a mutex in a lockBox
// that another goroutine of a unlocks, which leaves the first one
// recorded as its holder, and drops the box. Node a crashes, and node b
// waits for the mutex of a new box at the same address, if the memory is
// reused. The stale holder must not run on as a zombie of a.
func TestStaleHolder(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		b := w.AddNode("b", "10.0.0.2")
		var addr uintptr
		var ran atomic.Bool
		wake := make(chan bool)
		unlocked := make(chan bool)
		a.Start(func() {
			box := new(lockBox)
			addr = uintptr(unsafe.Pointer(&box.mu))
			box.mu.Lock()
			a.Go(func() {
				box.mu.Unlock()
				unlocked <- true
			})
			<-wake
			ran.Store(true)
		})
		<-unlocked
		var box *lockBox
		for i := 0; i < 1<<16 && box == nil; i++ {
			if i%1024 == 0 {
				runtime.GC()
			}
			if x := new(lockBox); uintptr(unsafe.Pointer(&x.mu)) == addr {
				box = x
			}
		}
		a.Kill()
		if box != nil {
			done := make(chan bool)
			b.Start(func() {
				box.mu.Lock()
				time.Sleep(time.Millisecond)
				box.mu.Unlock()
			})
			time.Sleep(time.Microsecond)
			b.Go(func() {
				box.mu.Lock()
				box.mu.Unlock()
				done <- true
			})
			<-done
		}
		close(wake)
		time.Sleep(time.Second)
		if ran.Load() {
			t.Fatal("a goroutine of node a ran after a crashed")
		}
		fmt.Println("reused:", box != nil)
	})
}

// TestPausedHolder pauses node a while it holds the mutex, so that its
// goroutine parks paused, then crashes a, and node b takes the mutex. b
// must get it from a zombie of a, although a's goroutine waits for a
// resume that never comes.
func TestPausedHolder(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		b := w.AddNode("b", "10.0.0.2")
		var mu sync.Mutex
		locked := make(chan bool)
		a.Start(func() {
			mu.Lock()
			locked <- true
			for range 1000 {
				runtime.Gosched()
			}
			mu.Unlock()
		})
		<-locked
		a.Pause()
		time.Sleep(time.Millisecond)
		a.Kill()
		done := make(chan bool)
		b.Start(func() {
			mu.Lock()
			mu.Unlock()
			done <- true
		})
		<-done
		fmt.Println("b took the lock after a crashed while paused")
		time.Sleep(time.Second)
	})
}
