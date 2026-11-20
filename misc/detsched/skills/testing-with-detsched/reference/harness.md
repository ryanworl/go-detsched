# Writing a harness

A harness is a Go test in its own package that runs the system under test (SUT) in a synctest bubble, drives a workload against it while faults happen, and checks what the SUT promises with `sdk/assert`. The README's "Simulating a cluster" section is the reference for what the simulation does; this file is how to use it.

## A library in one process

When the SUT is a library called in one process, a `synctest.Test` with goroutines drives it: every seed draws another interleaving. Draw the workload and options from the seed with `math/rand` or `sim.Intn`.

```go
func TestStore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		db := store.Open(store.Options{Shards: 1 + rand.IntN(4)}) // draw options per run
		var wg sync.WaitGroup
		for w := range 4 {
			wg.Go(func() {
				for i := range 200 {
					key, val := fmt.Sprintf("w%d-k%d", w, rand.IntN(8)), fmt.Sprint(i)
					if err := db.Put(key, val); err != nil {
						assert.Unreachable("put failed", map[string]any{"err": err.Error()})
						return
					}
					got, _ := db.Get(key) // a goroutine reads its own write
					assert.Always(got == val, "read returns own latest write", map[string]any{"key": key, "want": val, "got": got})
				}
			})
		}
		wg.Wait()
		db.Close()
	})
}
```

Then run it on one sim node, even if it does no I/O. On a node, CPU work takes simulated time as on a real machine; outside one it is free, so a loop that re-arms a timer for "now" spins at one instant until the search stops it, where a real machine would move on. A node also lets the search fork a library that writes files (it cannot fork a program that used the real file system), and lets crashes and disk faults reach it:

```go
if !sim.Supported() { // sim.NewWorld panics under stock Go
	t.Skip("requires GOEXPERIMENT=detsched")
}
w := sim.NewWorld(sim.Config{})
defer w.Close()
n := w.AddNode("db", "10.0.0.1")
done := n.Start(func() {
	db := store.Open("/data") // on the node's simulated disk
	lifecycle.SetupComplete(nil) // the simulation injects disk faults from here on
	// ... the workload, as above ...
})
<-done // closes when the function returns or the node crashes
```

- A node runs nothing before `Start`. Crash it from the test body with `n.Kill()` (power failure) or `n.KillProcess()`, and `Start` it again to reopen its files.
- Wait on the channel `Start` returns, not on a channel the node's code closes: a crash freezes the node's goroutines where they are.
- Disk faults (failed fsyncs, read errors, a full disk) start once setup completes, at `lifecycle.SetupComplete` or the first fsync, so that opening the store does not fail on an injected error. Call `lifecycle.SetupComplete` where your setup ends, once every party is ready: after the server has accepted the client's connection, not when the client's dial returns, or a fault during the half-finished handshake fails runs the way the bug would.
- A goroutine the library starts and its `Close` does not wait for fails the run with `blocked goroutines remain`; the summary names it. That is the SUT's behavior: wait for it in the harness (sleep past its timer after `Close`) so the search counts the bug you look for.

## A cluster

```go
func TestKV(t *testing.T) {
	if !sim.Supported() {
		t.Skip("requires GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{
			OnPanic: func(n *sim.Node, msg string, stack []byte) { // a SUT panic crashes only its node
				dump() // the harness's diagnostics, which otherwise die with the program
				assert.Unreachable("node panicked: "+sim.PanicKind(msg, stack), map[string]any{"node": n.Name()})
			},
		})
		defer w.Close()
		server := w.AddNode("server", "10.0.0.1")
		server.Start(serve)

		c := compose.New(w, compose.Config{
			Faults: sim.NemesisConfig{Restart: func(n *sim.Node) { n.Start(serve) }},
		})
		c.First("wait-for-server", func(x *compose.Ctx) error { ... })      // setup, no faults
		c.Parallel("put", func(x *compose.Ctx) error { ... })               // drivers, under faults
		c.Anytime("read-acked", func(x *compose.Ctx) error { ... })         // checks while faults run
		c.Eventually("takes-writes", func(x *compose.Ctx) error { ... })    // drivers killed, faults stopped
		c.Finally("acked-present", func(x *compose.Ctx) error { ... })      // drivers returned, faults stopped
		c.Run(t)
	})
}
```

- Set `OnPanic`, with `sim.PanicKind` in the message so that each kind of panic is its own group with its own replay line. Without it, a panic in the SUT ends the program before the harness's diagnostics run. The whole stack is in the run's log either way.
- Create the World first thing in the bubble, so that the fault timeline's times are the bubble's.
- Draw everything random from the seed (`sim.Intn`, `math/rand`), including configuration such as cluster size, options and timeouts, and print what was drawn in the failure dump. Runs forked after setup share what was drawn before it, and fresh seeds draw it anew, so keep `SetupComplete` where setup ends rather than moving it before the draws.
- Code that uses the network or the disk runs on a node: in `Start(f)`, goroutines `f` starts, or `Node.Go(f)`; compose runs drivers and checks on its client node (`x.Node()`). Nodes dial IP addresses.
- Read a SUT node's in-memory state from that node, with `n.Go(f)` and a timeout.
- Terminal checks (`Eventually`, `Finally`) run on forked branches of the timeline. A branch returns from `c.Run` after its check and runs the rest of the test body, so code after `c.Run` runs in every branch.
- `Eventually` kills the drivers. Restart SUT nodes from the nemesis's `Restart` or the harness's own loop, never from a driver, which could stop halfway through a restart.
- `x.Until(timeout, f)` returns early when the drivers' context ends, with f's last error: check its result. Goroutines a killed driver started may keep running; give harness state a generation number when a late writer could corrupt it.
- Join every goroutine the harness starts, and protect harness maps shared by goroutines with a mutex: goroutines are preempted anywhere.

## State across restarts

- A restart tests only what the SUT keeps across it. Keep a SUT's in-memory stores in the harness and hand them to the restarted node to model a disk that never loses an acknowledged write (enough for protocol bugs); use the SUT's file-based stores on the node's disk to let power failures and failed fsyncs reach it. An in-memory store written for unit tests is not crash-safe: when a failure starts in one, check the store before the SUT.
- Package variables (loggers, registries, pools, `sync.Once`) are one per test process, shared by every node and restart; see "Shared memory" in the README for the fixes.

## Locks and callbacks

- Never hold a harness lock across a call into the SUT, and never share one between nodes: a paused node holding it stalls the others, which reads as a SUT liveness bug. For a check across nodes, give each node its own recorder (a slice under its own mutex, or an `atomic.Pointer` to an immutable copy) and have the check read each.
- Use `sync.Mutex`, not a channel of capacity 1, for any lock a crashed node could hold.
- The nemesis's `Restart` and `Event` must return promptly and must not wait for a node or take a lock a node can hold.
- Give each node its own logger, and stamp lines with `w.Elapsed()`, the timeline's time, not the wall clock, which clock skips move.

## Faults and clocks

- To excuse a check during faults, ask `x.CalmSince()` (or `Nemesis.CalmSince()`), which says whether every fault has ended, and since when. Don't count faults from `Event`.
- Clock skips move wall clocks; `time.Since` and timers ignore them. A SUT that times leases by wall clock sees them end early or late by the skip.
- Each node's clock drifts: from setup's end it runs within ±1% of the simulation's, and a nemesis `clockrate` fault runs one at 1/4 to 4 times. A lease or timeout measured on one node and enforced on another must allow for that drift, as on real machines; don't call `SetClockRate(1)` to hide a failure that needs it. Compare times only within one node, and stamp harness checks with `w.Elapsed()`.
- Syncs take simulated time, up to seconds under the slow-sync faults, so a lease or election timeout can expire during one. Count a lease from when it was granted (`testdata/simlease` is an example of this bug).
- Disable a fault site (`buggify.Disable("sim.disk.fsyncfail")`) only when the SUT documents that it does not survive that fault, with a comment saying so.

## Checks that don't raise false alarms

- An operation that timed out, hit a crash, or returned an error may still have taken effect: track it as maybe applied and accept either outcome.
- A check's deadline must outlast the SUT's backoffs and timeouts (reconnect backoff, TCP retransmission, election timeouts after a partition heals); prefer "keeps making progress" to a fixed deadline.
- Messages a fault held back arrive late; a restarted node may get requests sent to its previous incarnation. After a power failure, recovery may roll back to any synced prefix.
- Read state the SUT updates concurrently at a consistent point.
- Once a known bug wedges a node, disarm the liveness checks it would fail.

## Checks that find the bug

- Assert the protocol's invariants where the SUT breaks them (one leader per term), not only what a client sees (replicas agree): a broken invariant fails in every run that breaks it, its symptom only in a few.
- Give each invariant its own message, and put the cause in a liveness check's message ("no leader for 30s", not "converges"). A compose check groups by its error's first line too, so start errors with what failed.
- Report a known problem you want counted but not failed with `assert.Sometimes`: the summary says how often it held, `NEVER TRUE` first.
- Print diagnostics once per run, after an assertion failed (`assert.Failures() > 0`), not per failure.

## Before a long search

- Plant a bug (drop an fsync, acknowledge before writing) and check that a short search (`detsched -duration 5m ./pkg`) catches it.
- Mark each error and timeout path you mean to exercise with `assert.Sometimes` at a place every run passes, and check the summary that it held.
- Time one replay: a checker that scans everything after each operation makes runs slow. The summary prints the slowest passing run's replay line when it took 5 seconds or more; profile it with `perf record -g` (`-test.cpuprofile` does not work under the experiment).
- Patch the SUT with a local copy and a `replace` directive in go.mod.

## Comparing the SUT before and after a fix

Search the package with each version and the same `-n`, giving the before version its own go.mod with `GOFLAGS`, so that each search builds its debug binary too (with `-exec`, debug lines run the optimized binary):

```
cd /abs/harness
GOFLAGS=-modfile=go.before.mod detsched -n 3000 .
detsched -n 3000 .
```

`go.before.mod` is a copy of go.mod whose `replace` points at an unchanged copy of the SUT, next to a copy of go.sum named `go.before.sum`. Compare failure rates, not seeds: a fix changes the schedules, and a fixed run goes on where the buggy one failed, so the same `-n` can take longer. A flag the harness reads works the same way with one package.
