// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package simfaults exercises the faults of detsched/sdk/sim one at a time
// and prints what a program sees: network faults in each mode, link
// baselines, throttling, clock skips, read-only real files, disk faults,
// and how TCP connections fail. TestSimFaults checks the lines
// it prints.
package simfaults

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/ryanworl/go-detsched/misc/detsched/sdk/buggify"
	"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim"
)

func logf(format string, args ...any) { fmt.Printf(format+"\n", args...) }

func stuckOnA(c chan struct{}) { <-c }
func stuckOnB(c chan struct{}) { <-c }

// echo serves lines back on addr.
func echo(addr string) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		logf("listen: %v", err)
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
					c.Close()
					return
				}
				c.Write([]byte(line))
			}
		}()
	}
}

func describe(err error) string {
	var errno syscall.Errno
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &errno):
		return errno.Error()
	case os.IsTimeout(err):
		return "deadline exceeded"
	}
	return err.Error()
}

// rtt sends a line on c and returns how long the echo takes, in ms.
func rtt(c net.Conn, r *bufio.Reader, deadline time.Duration) (int64, error) {
	start := time.Now()
	c.SetDeadline(start.Add(deadline))
	if _, err := c.Write([]byte("x\n")); err != nil {
		return 0, err
	}
	_, err := r.ReadString('\n')
	return time.Since(start).Milliseconds(), err
}

func TestFaults(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	// The faults under test, and no others.
	buggify.Disable("sim.net.retransmit", "sim.net.reset", "sim.disk.fsyncfail", "sim.disk.enospc", "sim.disk.readerror", "sim.disk.partialcrash", "sim.disk.torn", "sim.disk.slowsync")
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		b := w.AddNode("b", "10.0.0.2")
		c := w.AddNode("c", "10.0.0.3")
		for _, n := range []*sim.Node{a, b, c} {
			w.SetLink(a, n, sim.Link{Latency: 10 * time.Millisecond})
			w.SetLink(n, a, sim.Link{Latency: time.Millisecond})
		}
		a.Start(func() {})
		b.Start(func() { echo("10.0.0.2:7") })
		c.Start(func() { echo("10.0.0.3:7") })

		// as runs f as node a and waits for it.
		as := func(f func()) {
			done := make(chan bool)
			a.Go(func() { f(); done <- true })
			<-done
		}
		// Each start of a node is a process with its own process ID,
		// the same in every run (TestSimFaults compares the output).
		pid := func(n *sim.Node) int {
			ch := make(chan int)
			n.Go(func() { ch <- os.Getpid() })
			return <-ch
		}
		d := w.AddNode("d", "10.0.0.4")
		d.Start(func() {})
		pa, pb, pd := pid(a), pid(b), pid(d)
		d.KillProcess()
		d.Start(func() {})
		pd2 := pid(d)
		logf("process IDs a %d b %d d %d, d restarted %d", pa, pb, pd, pd2)
		logf("process IDs of nodes and restarts are distinct, and not the test's: %v", pa != pb && pb != pd && pd != pd2 && pa != os.Getpid())
		w.RemoveNode(d)

		var conn net.Conn
		var r *bufio.Reader
		as(func() {
			var err error
			conn, err = net.Dial("tcp", "10.0.0.2:7")
			if err != nil {
				t.Fatal(err)
			}
			r = bufio.NewReader(conn)
			ms, err := rtt(conn, r, time.Minute)
			logf("baseline rtt a->b: %dms %s", ms, describe(err))
		})

		f := w.PartitionGroups(sim.Effect{Mode: sim.Slowed, Mean: 100 * time.Millisecond}, false, []*sim.Node{a}, []*sim.Node{b, c})
		logf("fault: %v", f)
		as(func() {
			ms, err := rtt(conn, r, time.Minute)
			logf("slowed rtt a->b: %dms %s", ms, describe(err))
		})
		f.Heal()

		f = w.Clog(b, true, false, sim.Effect{Mode: sim.Jammed})
		logf("fault: %v", f)
		time.AfterFunc(35*time.Second, f.Heal)
		as(func() {
			ms, err := rtt(conn, r, time.Minute)
			logf("jammed 35s, past the TCP timeout of 30s: rtt %dms %s", ms, describe(err))
		})

		f = w.Cut(a, b, sim.Effect{Mode: sim.Stopped})
		logf("fault: %v", f)
		as(func() {
			ms, err := rtt(conn, r, time.Minute)
			logf("stopped a->b: rtt %dms %s", ms, describe(err))
		})
		f.Heal()
		as(func() {
			conn.Close()
			var err error
			conn, err = net.Dial("tcp", "10.0.0.2:7")
			if err != nil {
				t.Fatal(err)
			}
			r = bufio.NewReader(conn)
		})

		// A one-way cut lets the other direction through.
		f = w.Cut(b, a, sim.Effect{Mode: sim.Stopped})
		logf("fault: %v", f)
		as(func() {
			c2, err := net.Dial("tcp", "10.0.0.3:7")
			logf("dial a->c during cut b->a: %s", describe(err))
			if err == nil {
				ms, err := rtt(c2, bufio.NewReader(c2), time.Minute)
				logf("rtt a->c during cut b->a: %dms %s", ms, describe(err))
			}
		})
		f.Heal()

		// A connection attempt across a partition completes once it heals
		// before the connection attempt times out.
		f = w.Isolate(b)
		time.AfterFunc(time.Second, f.Heal)
		as(func() {
			start := time.Now()
			c2, err := net.Dial("tcp", "10.0.0.2:7")
			logf("dial across a partition healed after 1s: %s after %v", describe(err), time.Since(start).Round(100*time.Millisecond))
			if err == nil {
				c2.Close()
			}
		})

		// Lost packets show up as retransmission delays, from 200ms,
		// doubling for each further loss.
		w.SetLink(a, c, sim.Link{Latency: 10 * time.Millisecond, Drop: 1})
		as(func() {
			start := time.Now()
			_, err := net.Dial("tcp", "10.0.0.3:7")
			logf("dial with every packet a->c lost: %s after %v", describe(err), time.Since(start))
		})
		w.SetLink(a, c, sim.Link{Latency: 10 * time.Millisecond, Drop: 0.5})
		as(func() {
			c2, err := net.Dial("tcp", "10.0.0.3:7")
			if err != nil {
				t.Fatal(err)
			}
			var sum int64
			for range 10 {
				ms, _ := rtt(c2, bufio.NewReader(c2), time.Minute)
				sum += ms
			}
			logf("10 rtts with half the packets a->c lost take longer than 1s: %v", sum > 1000)
			c2.Close()
		})
		w.SetLink(a, c, sim.Link{Latency: 10 * time.Millisecond})

		// Throttling makes a node react late.
		total := func() int64 {
			var sum int64
			as(func() {
				for range 20 {
					ms, _ := rtt(conn, r, time.Minute)
					sum += ms
					time.Sleep(7 * time.Millisecond)
				}
			})
			return sum
		}
		base := total()
		b.Throttle(5)
		slow := total()
		b.Throttle(1)
		logf("throttle(5) slows b's replies: %v", slow > base+100)
		logf("unthrottled again: %v", total() == base)
		// A throttle that ends before its goroutine first runs leaves
		// the node running.
		b.Throttle(5)
		b.Throttle(1)
		logf("throttle ended at once: %v", total() == base)

		// Clock skips move the wall clock, not monotonic time.
		as(func() {
			t0 := time.Now()
			w.SkipClock(-time.Hour)
			t1 := time.Now()
			logf("skip back 1h: wall %v, since %v", t1.Round(0).Sub(t0.Round(0)), time.Since(t0))
			w.SkipClock(time.Hour)
		})

		// Nodes read real time zones.
		as(func() {
			_, err := time.LoadLocation("America/New_York")
			logf("load a real time zone: %s", describe(err))
		})

		// A paused node stays on the network: connections to it are
		// accepted by its kernel, but nothing answers.
		b.Pause()
		as(func() {
			_, err := rtt(conn, r, time.Second)
			logf("rtt to paused b: %s", describe(err))
			c2, err := net.DialTimeout("tcp", "10.0.0.2:7", time.Second)
			logf("dial paused b: %s", describe(err))
			if err == nil {
				c2.Close()
			}
		})
		b.Resume()

		disk(w, a)

	})
}

// disk exercises the disk faults on node n of w.
func disk(w *sim.World, n *sim.Node) {
	done := make(chan bool)
	run := func(f func()) {
		n.Go(func() { f(); done <- true })
		<-done
	}
	read := func(name string) string {
		data, err := os.ReadFile(name)
		if err != nil {
			return describe(err)
		}
		return fmt.Sprintf("%q", data)
	}
	run(func() {
		f, _ := os.Create("/gate")
		f.WriteString("synced ")
		logf("fsync: %s", describe(f.Sync()))
		f.WriteString("dropped")
		buggify.Force("sim.disk.fsyncfail", true)
		logf("fsync with a failing disk: %s", describe(f.Sync()))
		buggify.Force("sim.disk.fsyncfail", false)
		logf("read after the failed fsync: %s", read("/gate"))
		logf("fsync again: %s", describe(f.Sync()))
		f.Close()

		// A sync waits for the device, in simulated time, and keeps only
		// the calling goroutine waiting.
		f, _ = os.Create("/latency")
		f.WriteString("x")
		start := time.Now()
		f.Sync()
		logf("fsync waits for the device: %v", time.Since(start) > 0)
		o, _ := os.OpenFile("/osync", os.O_RDWR|os.O_CREATE|os.O_SYNC, 0o644)
		start = time.Now()
		o.WriteString("x")
		logf("an O_SYNC write waits for the device: %v", time.Since(start) > 0)
		o.Close()
		ticks, stop := 0, make(chan bool)
		go func() {
			for {
				select {
				case <-stop:
					return
				case <-time.After(time.Millisecond):
					ticks++
				}
			}
		}()
		f.WriteString("y")
		buggify.Force("sim.disk.slowsync", true)
		start = time.Now()
		f.Sync()
		slow := time.Since(start)
		buggify.Force("sim.disk.slowsync", false)
		close(stop)
		logf("sim.disk.slowsync stalls an fsync for 10ms or more: %v, while the node's other goroutines run: %v", slow >= 10*time.Millisecond, ticks >= int(slow/time.Millisecond)-1)
		f.Close()

		// Writeback writes whole pages: a later write to the page of a
		// failed one makes both durable.
		f, _ = os.Create("/page")
		f.WriteString("synced ")
		f.Sync()
		f.WriteString("dropped")
		buggify.Force("sim.disk.fsyncfail", true)
		f.Sync()
		buggify.Force("sim.disk.fsyncfail", false)
		f.WriteString(" redirtied")
		logf("fsync of a redirtied page: %s", describe(f.Sync()))
		f.Close()

		// An exempt file's fsync does not fail.
		n.SetSyncFaultExempt(".sst")
		f, _ = os.Create("/t.sst")
		f.WriteString("kept")
		buggify.Force("sim.disk.fsyncfail", true)
		logf("fsync of an exempt file with a failing disk: %s", describe(f.Sync()))
		buggify.Force("sim.disk.fsyncfail", false)
		n.SetSyncFaultExempt()
		f.Close()
	})
	n.Kill()
	n.Start(func() {})
	run(func() {
		logf("after a power failure: %s", read("/gate"))
		logf("redirtied page after a power failure: %s", read("/page"))

		// Latent sector errors last until the sector is rewritten. Reads
		// of unsynced data come from the page cache, so sync it first.
		f, _ := os.Create("/bad")
		f.Write(make([]byte, 2048))
		f.Sync()
		f.Close()
		w.SetCalm(true)
		buggify.Force("sim.disk.readerror", true)
		logf("read with a sector going bad in a calm: %s", describe(readErr("/bad")))
		w.SetCalm(false)
		logf("read with a sector going bad: %s", describe(readErr("/bad")))
		buggify.Force("sim.disk.readerror", false)
		logf("read again: %s", describe(readErr("/bad")))
		os.WriteFile("/bad", make([]byte, 2048), 0o644)
		logf("read after rewriting: %s", describe(readErr("/bad")))

		// A full disk.
		n.SetDiskFull(true)
		_, err := os.Create("/new")
		logf("create on a full disk: %s", describe(err))
		logf("append on a full disk: %s", describe(os.WriteFile("/gate", []byte("much longer than before"), 0o644)))
		n.SetDiskFull(false)
		logf("append after freeing space: %s", describe(os.WriteFile("/gate", []byte("much longer than before"), 0o644)))

		// A hard link names the same file, and survives the removal of
		// the original and, once its directory is synced, a power
		// failure.
		f, _ = os.Create("/orig")
		f.WriteString("linked")
		f.Sync()
		f.Close()
		err = os.Link("/orig", "/link")
		again := os.Link("/orig", "/link")
		os.Remove("/orig")
		if d, err := os.Open("/"); err == nil {
			d.Sync()
			d.Close()
		}
		logf("hard link: %s, again: %s, after removing the original: %s", describe(err), describe(again), read("/link"))

		// A write with O_SYNC is durable when it returns.
		f, _ = os.OpenFile("/osync", os.O_CREATE|os.O_WRONLY|os.O_SYNC, 0o644)
		f.WriteString("durable")
		f.Close()

		// Torn writes: each sector of an unsynced write survives a power
		// failure, or not, on its own.
		f, _ = os.Create("/torn")
		f.Sync()
		buf := make([]byte, 8*512)
		for i := range buf {
			buf[i] = 'x'
		}
		f.Write(buf)
		f.Close()
	})
	buggify.Force("sim.disk.torn", true)
	n.Kill()
	buggify.Force("sim.disk.torn", false)
	n.Start(func() {})
	run(func() {
		logf("O_SYNC write after a power failure: %s", read("/osync"))
		logf("hard link after a power failure: %s", read("/link"))
		data, _ := os.ReadFile("/torn")
		whole := true
		kept := 0
		for s := 0; s*512 < len(data); s++ {
			sector := data[s*512 : min(len(data), (s+1)*512)]
			n := 0
			for _, c := range sector {
				if c == 'x' {
					n++
				}
			}
			if n == len(sector) && n == 512 {
				kept++
			} else if n != 0 {
				whole = false
			}
		}
		logf("torn write: sectors are whole or lost: %v (%d of 8 kept)", whole, kept)

		// A file maps at the same address in every run, which the
		// program sees, as in package syscall's table of mappings.
		os.WriteFile("/mapped", []byte("mapped"), 0o644)
		f, _ := os.Open("/mapped")
		m, err := syscall.Mmap(int(f.Fd()), 0, 6, syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			logf("mmap: %s", describe(err))
		} else {
			logf("mmap: ok, reads %q at %p", m, &m[0])
			syscall.Munmap(m)
		}
		f.Close()
	})

	// Writes to a shared writable mapping reach the file: reads see
	// them, msync makes them durable, the mapping grows with mremap, a
	// process crash keeps what is unsynced, and a power failure loses it.
	var m []byte
	msync := func() error {
		_, _, e := syscall.Syscall(syscall.SYS_MSYNC, uintptr(unsafe.Pointer(&m[0])), uintptr(len(m)), 4) // MS_SYNC
		if e != 0 {
			return e
		}
		return nil
	}
	at := func(f *os.File, off int64) string {
		buf := make([]byte, 8)
		f.ReadAt(buf, off)
		return fmt.Sprintf("%q", bytes.TrimRight(buf, "\x00"))
	}
	run(func() {
		f, _ := os.OpenFile("/shared", os.O_RDWR|os.O_CREATE, 0o644)
		f.Truncate(8192)
		var err error
		m, err = syscall.Mmap(int(f.Fd()), 0, 8192, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		if err != nil {
			logf("shared mmap: %s", describe(err))
			return
		}
		copy(m, "synced")
		logf("msync: %s", describe(msync()))
		copy(m[4096:], "written")
		logf("read sees a write to a shared mapping: %s", at(f, 4096))
		f.Truncate(16384)
		if m, err = mremap(m, f, 16384); err != nil {
			logf("mremap: %s", describe(err))
			return
		}
		logf("mremap: the mapping keeps %q", m[4096:4103])
		copy(m[12288:], "grown")
		logf("msync: %s", describe(msync()))
		copy(m[100:], "kept")
	})

	// A page of a large mapping first written in a forked branch of the
	// timeline reaches the file there: the branch's write-back looks at
	// its own pages, not its parent's.
	var big []byte
	var bigf *os.File
	run(func() {
		bigf, _ = os.OpenFile("/big", os.O_RDWR|os.O_CREATE, 0o644)
		bigf.Truncate(1 << 20)
		big, _ = syscall.Mmap(int(bigf.Fd()), 0, 1<<20, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		copy(big, "head")
		logf("read of a big mapping's first page: %s", at(bigf, 0))
	})
	if sim.Branch("simfaults: a branch's write to a mapped page reaches the file") {
		var got string
		run(func() {
			copy(big[512<<10:], "branch")
			got = at(bigf, 512<<10)
		})
		if got != `"branch"` {
			fmt.Println("the branch reads", got)
			os.Exit(1)
		}
		os.Exit(0)
	}
	run(func() {
		logf("the branch's store stays in the branch: %s", at(bigf, 512<<10))
	})
	// A goroutine of no node, such as a finalizer, may unmap a mapping:
	// its writes reach the file, and later writes to the file leave its
	// memory alone.
	var un []byte
	var unf *os.File
	run(func() {
		unf, _ = os.OpenFile("/unmapped", os.O_RDWR|os.O_CREATE, 0o644)
		unf.Truncate(4096)
		un, _ = syscall.Mmap(int(unf.Fd()), 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		copy(un, "unmapped")
	})
	unmapped := make(chan error)
	go func() { unmapped <- syscall.Munmap(un) }()
	uerr := <-unmapped
	run(func() {
		got := at(unf, 0)
		_, werr := unf.WriteAt([]byte("again"), 0)
		logf("unmapped by a goroutine of no node: %s, %s, then a write: %s", describe(uerr), got, describe(werr))
		unf.Close()
	})
	n.KillProcess()
	n.Start(func() {})
	run(func() {
		f, _ := os.OpenFile("/shared", os.O_RDWR, 0)
		logf("after a process crash: %s %s %s %s", at(f, 0), at(f, 100), at(f, 4096), at(f, 12288))
		var err error
		if m, err = syscall.Mmap(int(f.Fd()), 0, 16384, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED); err != nil {
			logf("shared mmap: %s", describe(err))
			return
		}
		copy(m[200:], "lost")
	})
	n.Kill()
	n.Start(func() {})
	run(func() {
		f, _ := os.Open("/shared")
		logf("after a power failure: %s %s %s %s %s", at(f, 0), at(f, 100), at(f, 200), at(f, 4096), at(f, 12288))
		f.Close()
	})

	// Shared mappings of a file share its pages, as Linux's page cache
	// does: a store through one shows through every other at once, with
	// no system call in between, and reads and writes of the file see
	// the same bytes. A private mapping's stores stay its own. A process
	// crash keeps unsynced stores through either mapping, and a power
	// failure loses them.
	str := func(b []byte) string { return fmt.Sprintf("%q", bytes.TrimRight(b, "\x00")) }
	syncMap := func(b []byte) error {
		_, _, e := syscall.Syscall(syscall.SYS_MSYNC, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), 4) // MS_SYNC
		if e != 0 {
			return e
		}
		return nil
	}
	var a1, a2 []byte
	var got string
	run(func() {
		f, _ := os.OpenFile("/alias", os.O_RDWR|os.O_CREATE, 0o644)
		f.Truncate(8192)
		var err error
		if a1, err = syscall.Mmap(int(f.Fd()), 0, 8192, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED); err != nil {
			logf("shared mmap: %s", describe(err))
			return
		}
		ro, _ := syscall.Mmap(int(f.Fd()), 4096, 4096, syscall.PROT_READ, syscall.MAP_SHARED)
		priv, _ := syscall.Mmap(int(f.Fd()), 0, 8192, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_PRIVATE)
		copy(a1[4096:], "aliased")
		got = str(ro[:16])
		logf("a second shared mapping sees a store at once: %s", got)
		copy(priv[4096:], "private")
		got = str(a1[4096:4112]) + " " + str(ro[:16])
		logf("a private mapping's store stays its own: %s", got)
		logf("read sees a store through a mapping: %s", at(f, 4096))
		f.WriteAt([]byte("written"), 4100)
		logf("both mappings see a write: %s %s", str(a1[4096:4112]), str(ro[:16]))
		f.Truncate(4096)
		got = str(ro[:16])
		f.Truncate(8192)
		logf("a truncation clears the mapping: %s, and growing again: %s", got, str(ro[:16]))
		copy(a1[4096:], "after")
		got = str(ro[:16])
		logf("a store after a truncation: %s", got)
		f.Truncate(16384)
		if a1, err = mremap(a1, f, 16384); err != nil {
			logf("mremap: %s", describe(err))
			return
		}
		copy(a1[4096+8:], "moved")
		got = str(ro[8:16])
		logf("a store through a moved mapping: %s", got)
		syscall.Munmap(ro)
		syscall.Munmap(priv)
		priv, _ = syscall.Mmap(int(f.Fd()), 0, 16384, syscall.PROT_READ, syscall.MAP_PRIVATE)
		copy(a1[4300:], "stored")
		f.WriteAt([]byte("again"), 4200)
		logf("a private mapping after the last one went: %s %s", str(priv[4200:4208]), str(priv[4300:4308]))
		syscall.Munmap(priv)

		a2, _ = syscall.Mmap(int(f.Fd()), 0, 16384, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		copy(a1, "synced")
		logf("msync of an aliased mapping: %s", describe(syncMap(a2[:4096])))
		copy(a1[100:], "one")
		copy(a2[200:], "two")
		got = str(a2[100:104]) + " " + str(a1[200:204])
	})
	logf("unsynced stores through two mappings: %s", got)
	n.KillProcess()
	n.Start(func() {})
	run(func() {
		f, _ := os.OpenFile("/alias", os.O_RDWR, 0)
		logf("aliased after a process crash: %s %s %s", at(f, 0), at(f, 100), at(f, 200))
		a1, _ = syscall.Mmap(int(f.Fd()), 0, 16384, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		a2, _ = syscall.Mmap(int(f.Fd()), 0, 16384, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
		copy(a1[300:], "three")
		copy(a2[400:], "four")
		logf("aliased mappings after a restart: %s %s", str(a2[300:308]), str(a1[400:408]))
	})
	n.Kill()
	n.Start(func() {})
	run(func() {
		f, _ := os.Open("/alias")
		logf("aliased after a power failure: %s %s %s %s %s", at(f, 0), at(f, 100), at(f, 200), at(f, 300), at(f, 400))
		f.Close()
	})
}

func readErr(name string) error {
	_, err := os.ReadFile(name)
	return err
}

// TestPanic checks that with Config.OnPanic a panic on a node's
// goroutine, or a fatal error such as unlocking an unlocked mutex, crashes
// only that node, as KillProcess would, after Config.OnPanic sees it.
func TestPanic(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		spins := 0
		w := sim.NewWorld(sim.Config{OnPanic: func(n *sim.Node, msg string, _ []byte) {
			logf("OnPanic: %s: %s", n.Name(), msg)
			if msg == "while spinning" {
				before := spins
				for range 10 {
					runtime.Gosched()
				}
				logf("the node ran during OnPanic: %v", spins != before)
			}
		}})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		b := w.AddNode("b", "10.0.0.2")
		b.Start(func() { echo("10.0.0.2:7") })
		a.Start(func() {
			go func() {
				time.Sleep(time.Second)
				var m map[string]int
				m["x"] = 1 // a runtime error, on a goroutine the node started
			}()
			time.Sleep(time.Hour)
		})
		time.Sleep(2 * time.Second)
		logf("after a panic on a: a up %v, b up %v", a.Up(), b.Up())
		a.Start(func() {})
		done := make(chan bool)
		a.Go(func() {
			c, err := net.Dial("tcp", "10.0.0.2:7")
			if err == nil {
				ms, err := rtt(c, bufio.NewReader(c), time.Minute)
				logf("restarted a reaches b: %dms %s", ms, describe(err))
			}
			done <- true
		})
		<-done
		a.Go(func() {
			var mu sync.Mutex
			mu.Unlock()
		})
		time.Sleep(time.Second)
		logf("after a fatal error on a: a up %v, b up %v", a.Up(), b.Up())
		a.Start(func() {})
		a.Go(func() {
			var mu sync.Mutex
			defer mu.Unlock() // raises a fatal error as the panic unwinds
			panic(errors.New("boom"))
		})
		time.Sleep(time.Second)
		a.Start(func() {})
		a.Go(func() { os.Exit(3) }) // as log.Fatal does
		time.Sleep(time.Second)
		logf("after os.Exit on a: a up %v, b up %v", a.Up(), b.Up())
		a.Start(func() {})
		a.Go(func() {
			for {
				spins++
				runtime.Gosched()
			}
		})
		a.Go(func() { panic("while spinning") })
		time.Sleep(time.Second)
	})
}

// TestTCP checks how simulated TCP connections fail.
func TestTCP(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	buggify.Disable("sim.net.retransmit", "sim.net.reset")
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		a := w.AddNode("a", "10.0.0.1")
		b := w.AddNode("b", "10.0.0.2")
		for _, l := range [][2]*sim.Node{{a, b}, {b, a}} {
			w.SetLink(l[0], l[1], sim.Link{Latency: 10 * time.Millisecond})
		}
		as := func(n *sim.Node, f func()) {
			done := make(chan bool)
			n.Go(func() { f(); done <- true })
			<-done
		}
		// accepted serves one connection on b with f.
		accepted := func(f func(net.Conn)) {
			b.Go(func() {
				l, err := net.Listen("tcp", "10.0.0.2:8")
				if err != nil {
					logf("listen: %v", err)
					return
				}
				c, err := l.Accept()
				l.Close()
				if err == nil {
					f(c)
					c.Close()
				}
			})
		}
		a.Start(func() {})
		b.Start(func() {})

		// A failed connect releases its port, and a close releases a
		// port the socket was bound to.
		as(a, func() {
			fd, _ := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
			syscall.Connect(fd, &syscall.SockaddrInet4{Port: 9, Addr: [4]byte{10, 0, 0, 2}})
			time.Sleep(100 * time.Millisecond)
			sa, _ := syscall.Getsockname(fd)
			fd2, _ := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
			logf("bind to the port of a refused connect: %s", describe(syscall.Bind(fd2, sa)))
			syscall.Close(fd2)
			syscall.Close(fd)
			d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 5000}}
			_, err1 := d.Dial("tcp", "10.0.0.2:9")
			_, err2 := d.Dial("tcp", "10.0.0.2:9")
			logf("dial twice from port 5000 to a closed port: %s, %s", describe(err1), describe(err2))
		})

		// Data arrives in segments, and a writer can have only so much
		// in flight.
		accepted(func(c net.Conn) { c.Write(make([]byte, 64<<10)) })
		as(a, func() {
			c, err := net.Dial("tcp", "10.0.0.2:8")
			if err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, 64<<10)
			n, err := c.Read(buf)
			logf("the first read of a 64KiB write gets part of it: %v %s", n < len(buf), describe(err))
			c.Close()
		})
		accepted(func(c net.Conn) { time.Sleep(time.Minute) })
		as(a, func() {
			c, err := net.Dial("tcp", "10.0.0.2:8")
			if err != nil {
				t.Fatal(err)
			}
			f := w.Cut(a, b, sim.Effect{Mode: sim.Stopped})
			c.SetWriteDeadline(time.Now().Add(time.Second))
			n, err := c.Write(make([]byte, 8<<20))
			logf("write 8MiB across a stopped link: %d bytes, %s", n, describe(err))
			f.Heal()
			c.Close()
		})
		// A reader that does not read holds up its writer once what it
		// has not read and what is in flight fill the windows.
		read := make(chan bool)
		accepted(func(c net.Conn) {
			<-read
			buf := make([]byte, 1<<20)
			for {
				if _, err := c.Read(buf); err != nil {
					return
				}
			}
		})
		as(a, func() {
			c, err := net.Dial("tcp", "10.0.0.2:8")
			if err != nil {
				t.Fatal(err)
			}
			c.SetWriteDeadline(time.Now().Add(time.Second))
			n, err := c.Write(make([]byte, 16<<20))
			logf("write 16MiB to a reader that does not read: %d bytes, %s", n, describe(err))
			close(read)
			c.SetWriteDeadline(time.Now().Add(time.Second))
			n, err = c.Write(make([]byte, 16<<20))
			logf("once it reads: %d bytes, %s", n, describe(err))
			c.Close()
		})

		// When TCP gives up retransmitting, only the sending end fails,
		// and it can still read what had arrived. The other end learns
		// of it when it sends.
		var ca, cb net.Conn
		got, release := make(chan bool), make(chan bool)
		accepted(func(c net.Conn) {
			cb = c
			c.Write([]byte("hello"))
			got <- true
			<-release
		})
		as(a, func() {
			var err error
			if ca, err = net.Dial("tcp", "10.0.0.2:8"); err != nil {
				t.Fatal(err)
			}
		})
		<-got
		time.Sleep(100 * time.Millisecond)
		f := w.Cut(a, b, sim.Effect{Mode: sim.Stopped})
		as(a, func() {
			ca.Write([]byte("x"))
			time.Sleep(3 * time.Second)
			buf := make([]byte, 16)
			n, err := ca.Read(buf)
			logf("the timed-out end reads what had arrived: %q %s", buf[:n], describe(err))
			_, err = ca.Read(buf)
			logf("then its error: ETIMEDOUT %v", errors.Is(err, syscall.ETIMEDOUT))
			_, err = ca.Read(buf)
			logf("then: %s", describe(err))
		})
		as(b, func() {
			cb.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			_, err := cb.Read(make([]byte, 16))
			logf("the other end has not failed: %s", describe(err))
		})
		f.Heal()
		as(b, func() {
			_, err1 := cb.Write([]byte("y"))
			time.Sleep(100 * time.Millisecond)
			_, err2 := cb.Write([]byte("y"))
			_, err3 := cb.Write([]byte("y"))
			logf("the other end writes, is reset, and writes twice more: %s, %s, %s", describe(err1), describe(err2), describe(err3))
		})
		close(release)
		ca.Close()

		// TCP keepalive, which package net turns on with 15s idle and
		// 15s between 9 probes: an idle connection to a peer that lost
		// power fails with ETIMEDOUT once the probes go unanswered, and
		// one to a peer that is up again is reset by the first probe.
		for _, restart := range []bool{false, true} {
			accepted(func(c net.Conn) { time.Sleep(time.Hour) })
			as(a, func() {
				c, err := net.Dial("tcp", "10.0.0.2:8")
				if err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Second)
				b.Kill()
				if restart {
					b.Start(func() {})
				}
				start := time.Now()
				_, err = c.Read(make([]byte, 1))
				logf("idle read from b after a power failure (restarted %v): %s after %v", restart, describe(err), time.Since(start).Round(time.Second))
				c.Close()
			})
			if !restart {
				b.Start(func() {})
			}
		}

		// Stacks shows the goroutines of a node that Goroutines counts,
		// and no other node's.
		hold := make(chan struct{})
		b.Go(func() { stuckOnB(hold) })
		a.Go(func() { stuckOnA(hold) })
		time.Sleep(time.Millisecond)
		st := b.Stacks()
		shown := len(regexp.MustCompile(`(?m)^goroutine \d+ \[`).FindAllString(st, -1))
		logf("stacks of b: as many as Goroutines %v, shows b's %v, shows a's %v", shown == b.Goroutines(), strings.Contains(st, "stuckOnB"), strings.Contains(st, "stuckOnA"))
		close(hold)

		// A node whose process crashed still has a kernel, which refuses
		// connections at once; after a power failure, nothing answers.
		b.KillProcess()
		as(a, func() {
			start := time.Now()
			_, err := net.Dial("tcp", "10.0.0.2:8")
			logf("dial b after its process crashed: %s after %v", describe(err), time.Since(start))
		})
		b.Start(func() {})
		b.Kill()
		as(a, func() {
			_, err := net.DialTimeout("tcp", "10.0.0.2:8", time.Second)
			logf("dial b after a power failure: %s", describe(err))
		})
		b.Start(func() {})

		// A client that loses power during the handshake leaves the
		// server a connection that fails once the client is back.
		accepted(func(c net.Conn) {
			for {
				if _, err := c.Write([]byte("z")); err != nil {
					logf("the server's writes to a client that lost power mid-handshake: %s", describe(err))
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		})
		time.Sleep(time.Millisecond)
		a.Go(func() { net.Dial("tcp", "10.0.0.2:8") })
		time.Sleep(15 * time.Millisecond) // the SYN arrived, the SYN-ACK did not
		a.Kill()
		time.Sleep(500 * time.Millisecond)
		a.Start(func() {})
		time.Sleep(time.Second)

		// The bubble goes on after the world closes, with the world's
		// events still pending.
		a.Go(func() { net.Dial("tcp", "10.0.0.2:8") })
		time.Sleep(time.Millisecond)
		w.Close()
		time.Sleep(time.Minute)
		logf("the bubble goes on after the world closed: ok")
	})
}

// TestUDP checks simulated UDP: latency, the ECN bits and segmentation
// offload of sendmsg, truncation, MTUs, faults, and the receive buffer.
func TestUDP(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	buggify.Disable("sim.net.udpdrop", "sim.net.udpdup", "sim.net.udpreorder", "sim.net.ecnce", "sim.net.ecnbleach", "sim.net.ecnmangle")
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		b := w.AddNode("b", "10.0.0.2")
		for _, l := range [][2]*sim.Node{{a, b}, {b, a}} {
			w.SetLink(l[0], l[1], sim.Link{Latency: 10 * time.Millisecond})
		}
		a.Start(func() {})
		b.Start(func() {})
		as := func(n *sim.Node, f func()) {
			done := make(chan bool)
			n.Go(func() { f(); done <- true })
			<-done
		}
		var ca, cb *net.UDPConn
		as(a, func() {
			var err error
			if ca, err = net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 4000}); err != nil {
				t.Fatal(err)
			}
		})
		as(b, func() {
			var err error
			if cb, err = net.ListenUDP("udp4", &net.UDPAddr{Port: 4001}); err != nil {
				t.Fatal(err)
			}
			raw, _ := cb.SyscallConn()
			raw.Control(func(fd uintptr) {
				syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, ipRecvTOS, 1)
				syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_PKTINFO, 1)
			})
		})
		toB := &net.UDPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 4001}
		// recv reads one datagram on b within d.
		recv := func(size int, d time.Duration) (n int, tos byte, from *net.UDPAddr, flags int, err error) {
			buf, oob := make([]byte, size), make([]byte, 128)
			cb.SetReadDeadline(time.Now().Add(d))
			n, oobn, flags, from, err := cb.ReadMsgUDP(buf, oob)
			msgs, _ := syscall.ParseSocketControlMessage(oob[:oobn])
			for _, m := range msgs {
				if m.Header.Level == syscall.IPPROTO_IP && m.Header.Type == syscall.IP_TOS {
					tos = m.Data[0]
				}
			}
			return n, tos, from, flags, err
		}
		as(a, func() {
			start := time.Now()
			ca.WriteToUDP([]byte("hi"), toB)
			n, _, from, flags, err := recv(16, time.Second)
			logf("udp a->b: %d bytes from %v after %v %s", n, from, time.Since(start), describe(err))

			// ECN bits travel in a control message.
			oob := make([]byte, syscall.CmsgSpace(1))
			h := (*syscall.Cmsghdr)(unsafe.Pointer(&oob[0]))
			h.Level, h.Type = syscall.IPPROTO_IP, syscall.IP_TOS
			h.SetLen(syscall.CmsgLen(1))
			oob[syscall.CmsgLen(0)] = 2 // ECT(0)
			ca.WriteMsgUDP([]byte("ecn"), oob, toB)
			_, tos, _, _, err := recv(16, time.Second)
			logf("udp ecn bits arrive: %d %s", tos, describe(err))

			ca.WriteToUDP(make([]byte, 1000), toB)
			n, _, _, flags, err = recv(100, time.Second)
			logf("udp truncated read: %d, MSG_TRUNC %v %s", n, flags&syscall.MSG_TRUNC != 0, describe(err))

			_, err = ca.WriteToUDP(make([]byte, 1473), toB)
			logf("udp send over the interface MTU: %s", describe(err))

			// Segmentation offload: one sendmsg, several datagrams.
			oob = make([]byte, syscall.CmsgSpace(2))
			h = (*syscall.Cmsghdr)(unsafe.Pointer(&oob[0]))
			h.Level, h.Type = syscall.IPPROTO_UDP, 103 // UDP_SEGMENT
			h.SetLen(syscall.CmsgLen(2))
			*(*uint16)(unsafe.Pointer(&oob[syscall.CmsgLen(0)])) = 1000
			ca.WriteMsgUDP(make([]byte, 2500), oob, toB)
			var sizes []int
			for range 3 {
				n, _, _, _, _ := recv(2000, time.Second)
				sizes = append(sizes, n)
			}
			slices.Sort(sizes)
			logf("udp gso 2500 bytes in segments of 1000: %v", sizes)

			w.SetLink(a, b, sim.Link{Latency: 10 * time.Millisecond, MTU: 1280})
			ca.WriteToUDP(make([]byte, 1300), toB)
			_, _, _, _, err = recv(2000, time.Second)
			logf("udp datagram over the path MTU: %s", describe(err))
			w.SetLink(a, b, sim.Link{Latency: 10 * time.Millisecond})

			f := w.Cut(a, b, sim.Effect{Mode: sim.Stopped})
			ca.WriteToUDP([]byte("x"), toB)
			time.Sleep(time.Second)
			f.Heal()
			_, _, _, _, err = recv(16, time.Second)
			logf("udp across a stopped link: %s", describe(err))

			// A datagram in flight when the link stops is lost with it.
			ca.WriteToUDP([]byte("x"), toB)
			time.Sleep(5 * time.Millisecond)
			f = w.Cut(a, b, sim.Effect{Mode: sim.Stopped})
			time.Sleep(time.Second)
			f.Heal()
			_, _, _, _, err = recv(16, time.Second)
			logf("udp in flight when the link stops: %s", describe(err))

			// Behind a NAT, a sends from a public port, which b can answer,
			// until a rebinding gives a a new one.
			a.SetNAT(true)
			ca.WriteToUDP([]byte("n"), toB)
			_, _, pub, _, err := recv(16, time.Second)
			logf("udp from behind a NAT: from a public port %v %s", pub != nil && pub.Port != 4000, describe(err))
			cb.WriteToUDP([]byte("r"), pub)
			ca.SetReadDeadline(time.Now().Add(time.Second))
			_, err = ca.Read(make([]byte, 16))
			logf("udp a reply to the public port: %s", describe(err))
			a.Rebind()
			ca.WriteToUDP([]byte("n"), toB)
			_, _, pub2, _, err := recv(16, time.Second)
			logf("udp after a rebinding: from a new port %v %s", pub2 != nil && pub != nil && pub2.Port != pub.Port, describe(err))
			cb.WriteToUDP([]byte("r"), pub)
			ca.SetReadDeadline(time.Now().Add(time.Second))
			_, err = ca.Read(make([]byte, 16))
			logf("udp a reply to the old public port: %s", describe(err))
			a.SetNAT(false)

			f = w.Cut(a, b, sim.Effect{Mode: sim.Jammed})
			start = time.Now()
			ca.WriteToUDP([]byte("x"), toB)
			time.Sleep(time.Second)
			f.Heal()
			_, _, _, _, err = recv(16, time.Second)
			logf("udp across a jammed link healed after 1s: after %v %s", time.Since(start), describe(err))

			// A full receive buffer drops datagrams, and ECN-capable ones
			// are marked Congestion Experienced as the queue builds.
			oob = make([]byte, syscall.CmsgSpace(1))
			h = (*syscall.Cmsghdr)(unsafe.Pointer(&oob[0]))
			h.Level, h.Type = syscall.IPPROTO_IP, syscall.IP_TOS
			h.SetLen(syscall.CmsgLen(1))
			oob[syscall.CmsgLen(0)] = 2
			for range 1000 {
				ca.WriteMsgUDP(make([]byte, 1000), oob, toB)
			}
			got, ce := 0, 0
			for {
				_, tos, _, _, err := recv(2000, time.Second)
				if err != nil {
					break
				}
				got++
				if tos == 3 {
					ce++
				}
			}
			logf("udp a full buffer drops datagrams: %v, marks some CE: %v", got < 1000, ce > 0 && ce < got)
		})
		// recvmmsg reads several datagrams at once.
		for range 3 {
			as(a, func() { ca.WriteToUDP([]byte("m"), toB) })
		}
		time.Sleep(100 * time.Millisecond)
		as(b, func() { recvmmsg(cb) })
		// A node that is down, or a port nothing listens on, loses the
		// datagram; the sender learns nothing.
		b.KillProcess()
		as(a, func() {
			_, err := ca.WriteToUDP([]byte("x"), toB)
			logf("udp to a node whose process crashed: %s", describe(err))
		})
	})
}

// TestForwardSkips checks that with FaultClockSkipBack disabled the
// nemesis's clock skips never move the wall clock back.
func TestForwardSkips(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		w.AddNode("a", "10.0.0.1")
		var skips int
		var last time.Duration
		cfg := sim.NemesisConfig{
			Disable: []string{sim.FaultPartition, sim.FaultClog, sim.FaultLink, sim.FaultRestore, sim.FaultPause, sim.FaultThrottle, sim.FaultClockSkipBack},
			Event: func(ev sim.Event) {
				if ev.Kind != sim.FaultClockSkip {
					return
				}
				skips++
				if s := w.ClockSkew(); s < last {
					t.Errorf("clock skew went back from %v to %v (%s)", last, s, ev.Desc)
				} else {
					last = s
				}
			},
		}
		// Each nemesis draws a run's shape, and some runs have no
		// faults, or storms without clock skips.
		for i := 0; i < 10 && skips < 10; i++ {
			nm := w.NewNemesis(cfg)
			nm.Start()
			time.Sleep(time.Hour)
			nm.Stop()
		}
		if skips < 10 {
			t.Errorf("%d clock skips in 10 nemesis runs of an hour, want at least 10", skips)
		}
	})
}

// TestDiskFaults checks the nemesis's disk faults: an fsync on a stalled
// disk returns when the fault ends, also when a calm ends it early, while
// the node's other goroutines and the other nodes run on, and fsyncs on a
// slowed disk take longer.
func TestDiskFaults(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	buggify.Disable("sim.disk.slowsync")
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.1")
		b := w.AddNode("b", "10.0.0.2")
		type span struct {
			start, end time.Time
			stall      bool
		}
		var faults []span
		var syncs []span
		var ticksA, ticksB []time.Time
		tick := func(ts *[]time.Time) func() {
			return func() {
				for range time.Tick(5 * time.Millisecond) {
					*ts = append(*ts, time.Now())
				}
			}
		}
		a.Start(func() {
			go tick(&ticksA)()
			f, _ := os.Create("/log")
			for {
				f.WriteString("entry\n")
				start := time.Now()
				f.Sync()
				syncs = append(syncs, span{start: start, end: time.Now()})
				time.Sleep(time.Millisecond)
			}
		})
		b.Start(tick(&ticksB))
		cfg := sim.NemesisConfig{
			Nodes:   []*sim.Node{a},
			Disable: []string{sim.FaultPartition, sim.FaultClog, sim.FaultLink, sim.FaultRestore, sim.FaultKill, sim.FaultKillProcess, sim.FaultStop, sim.FaultPause, sim.FaultThrottle, sim.FaultClockSkip},
			Event: func(ev sim.Event) {
				switch {
				case ev.Kind != sim.FaultDisk:
				case ev.Start:
					faults = append(faults, span{start: time.Now(), stall: strings.HasPrefix(ev.Desc, "disk stalled")})
				default:
					faults[len(faults)-1].end = time.Now()
				}
			},
		}
		// Each nemesis draws a run's shape, and some runs have no
		// faults, or few disk faults.
		long := func(f span) bool { return f.stall && f.end.Sub(f.start) > 20*time.Millisecond }
		slowed := func(f span) bool { return !f.stall }
		for i := 0; i < 10 && !(slices.ContainsFunc(faults, long) && slices.ContainsFunc(faults, slowed)); i++ {
			nm := w.NewNemesis(cfg)
			nm.Start()
			time.Sleep(5 * time.Minute)
			nm.Stop()
		}
		in := func(ts []time.Time, f span) bool {
			return slices.ContainsFunc(ts, func(t time.Time) bool { return t.After(f.start) && t.Before(f.end) })
		}
		stalls, stalledSyncs, wake, ran := 0, 0, true, true
		var slowSum, slowN, fastSum, fastN time.Duration
		for _, s := range syncs {
			i := slices.IndexFunc(faults, func(f span) bool { return !s.start.Before(f.start) && s.start.Before(f.end) })
			switch {
			case i < 0:
				fastSum, fastN = fastSum+s.end.Sub(s.start), fastN+1
			case faults[i].stall:
				stalledSyncs++
				wake = wake && s.end.Equal(faults[i].end)
			default:
				slowSum, slowN = slowSum+s.end.Sub(s.start), slowN+1
			}
		}
		for _, f := range faults {
			if long(f) {
				stalls++
				ran = ran && in(ticksA, f) && in(ticksB, f)
			}
		}
		logf("disk stalls: %v, their fsyncs return when the stall ends: %v, while a's other goroutines and b run: %v", stalls > 0 && stalledSyncs > 0, wake, ran)
		logf("fsyncs on a slowed disk take 5 times as long or more: %v", slowN > 0 && fastN > 0 && slowSum/slowN >= 5*(fastSum/fastN))
	})
}

// TestNetlink checks that a node lists its interfaces, addresses, and
// routes through netlink route sockets, as net.Interfaces and route
// libraries do.
func TestNetlink(t *testing.T) {
	if !sim.Supported() || runtime.GOOS != "linux" {
		t.Skip("needs GOEXPERIMENT=detsched on Linux")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		a := w.AddNode("a", "10.0.0.7")
		done := make(chan bool)
		a.Start(func() {})
		a.Go(func() {
			defer close(done)
			ifs, err := net.Interfaces()
			if err != nil || len(ifs) != 2 || ifs[0].Name != "lo" || ifs[1].Name != "eth0" || ifs[1].MTU != 1500 || ifs[1].Flags&net.FlagUp == 0 {
				t.Errorf("Interfaces() = %v, %v; want lo and eth0", ifs, err)
			}
			addrs, err := net.InterfaceAddrs()
			if err != nil || len(addrs) != 2 || addrs[1].String() != "10.0.0.7/8" {
				t.Errorf("InterfaceAddrs() = %v, %v; want 127.0.0.1/8 and 10.0.0.7/8", addrs, err)
			}
			checkRoutes(t)
		})
		<-done
	})
}

// TestExclude checks that the nemesis never hits a node in
// NemesisConfig.Exclude with a node fault, while its network faults reach
// it: a partition can put it with some servers and not others, and it can
// be clogged or be an end of a faulty link.
func TestExclude(t *testing.T) {
	if !sim.Supported() {
		t.Skip("needs GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
		defer w.Close()
		servers := []*sim.Node{w.AddNode("s1", "10.0.0.1"), w.AddNode("s2", "10.0.0.2"), w.AddNode("s3", "10.0.0.3")}
		c := w.AddNode("c", "10.0.0.4")
		for _, n := range append(servers, c) {
			n.Start(func() {})
		}
		var hitC, hitServers, split, link, clog bool
		cfg := sim.NemesisConfig{
			Exclude: []*sim.Node{c},
			Restart: func(n *sim.Node) { n.Start(func() {}) },
			Event: func(ev sim.Event) {
				if !ev.Start {
					return
				}
				switch ev.Kind {
				case sim.FaultPartition:
					// Some server shares c's group, which no fault
					// covers in either direction, and some does not.
					apart := func(s *sim.Node) bool { return ev.Net.Covers(c, s) || ev.Net.Covers(s, c) }
					together := func(s *sim.Node) bool { return !apart(s) }
					split = split || slices.ContainsFunc(servers, apart) && slices.ContainsFunc(servers, together)
				case sim.FaultLink:
					link = link || slices.ContainsFunc(servers, func(s *sim.Node) bool { return ev.Net.Covers(c, s) || ev.Net.Covers(s, c) })
				case sim.FaultClog:
					clog = clog || ev.Node == c
				default:
					hitC = hitC || ev.Node == c
					hitServers = hitServers || slices.Contains(servers, ev.Node)
				}
			},
		}
		// Each nemesis draws a run's shape, and some runs have no
		// faults, or storms without network faults.
		for i := 0; i < 10 && !(hitServers && split && link && clog); i++ {
			nm := w.NewNemesis(cfg)
			nm.Start()
			time.Sleep(time.Hour)
			nm.Stop()
		}
		logf("an excluded node: node faults hit it: %v, hit servers: %v, partitioned with some servers and not others: %v, an end of a faulty link: %v, clogged: %v", hitC, hitServers, split, link, clog)
	})
}
