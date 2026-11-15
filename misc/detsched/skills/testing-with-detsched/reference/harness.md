# Writing a harness with sim and compose

A harness is a Go test in its own package that runs the system under test (SUT) on simulated nodes, drives a workload against it while faults happen, and checks what the SUT promises. `testdata/simcompose` in the detsched repository is a complete small example: read it before writing a new one.

## Contents

- Shape of a harness
- Nodes, goroutines, and shared state
- Locks and callbacks
- Faults and clocks
- Writing checks that don't raise false alarms
- Before the first long search

## Shape of a harness

```go
func TestKV(t *testing.T) {
	if !sim.Supported() {
		t.Skip("requires GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{})
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

- Create the World first thing in the bubble. The fault timeline's times are seconds since the World was created, and dlv's `runtime.curg.bubble.now` lines up with them only when the bubble and the World start together.
- Draw everything random from the seed: `sim.Intn`, `sim.Rand`, `math/rand`, or map order all come from `GODETSEED`. Draw configuration per run (cluster size, SUT options, timeouts) so the search explores it, and record what was drawn in the harness's failure dump.
- Terminal checks (`Eventually`, `Finally`) also run on branches of the timeline: compose forks the process at random points, the branch runs the check, and the timeline goes on. A failure there names its branch, which `GODETBRANCH` replays.
- `Eventually` kills the drivers by killing the client node they run on. A driver that was in the middle of restarting a SUT node (Stop then Start) stops halfway, and the check sees a node that is up without its server. Restart SUT nodes from the harness's own loop or from the nemesis's `Restart`, not from a driver.
- `x.Until(timeout, f)` returns early when the drivers' context ends, with f's last error. Check its result.
- Drivers that compose killed keep their goroutines frozen, and goroutines a driver started (an RPC past its timeout) may keep running. Give harness state a generation number when a late writer could corrupt it.

## Nodes, goroutines, and shared state

- Code that uses the network or the disk must run on a node: in `Node.Start(f)`, in goroutines `f` starts, or in `Node.Go(f)`. The test body and its goroutines belong to no node; their socket calls fail with `EPERM`. Compose runs drivers and checks on its client node (`x.Node()`).
- Nodes have no name resolution: dial IP addresses.
- Read a SUT node's in-memory state from that node, with `n.Go(f)` and a timeout, not from the test body. A goroutine of no node that blocks on a lock a crashed node held never wakes.
- Goroutines are still preempted anywhere. Harness maps and slices shared by goroutines need a mutex, or the run dies with `concurrent map writes`.
- Join every goroutine the harness starts before the test body returns, or synctest reports a deadlock.
- Package variables of the SUT and its dependencies are shared by every node and every restart: the README's "Shared state" bullet in "Simulating a cluster" lists the common ones and the fixes (an instance per node, global state registered once before starting nodes, per-process state keyed on `os.Getpid()`, which differs between nodes and between restarts of one node). A SUT that registers a global per server (one HTTP pool, one metrics registry) needs a small patch.
- A package that captures `time.Now()` in a package variable at init, before the bubble, and later compares bubble time with it makes runs differ from each other. Nothing reports the read itself: it shows up only as `detsched`'s warning that seed 1 ran two different schedules, and as replays that don't reproduce. Take such values inside the bubble, or patch the SUT.
- A cleanup or finalizer that stops a timer created in the bubble kills the run (`stop of synctest timer from outside bubble`), as in stock synctest.

## Locks and callbacks

- Never hold a harness lock across a call into the SUT, and never share one harness lock between nodes: when the nemesis pauses a node whose goroutine holds it, every other node that takes it stalls until the pause ends, which reads as a liveness bug in the SUT. Keep locks per node, or use atomics or lock-free rings.
- The nemesis calls `Restart` and `Event` on its own goroutine, which alone resumes paused nodes. They must return promptly and must not take any lock a node's goroutine can hold, nor wait for a node. The runtime fails the run with the callback's stack when one does.
- Don't log from nodes through a shared `log.Logger` or a library's global logger: a node that crashes while holding the logger's mutex wedges every node that logs. Give each node its own logger, or write to a lock-free ring.

## Faults and clocks

- To excuse checks during faults, ask whether the run is in a calm: `x.CalmSince()` in a compose command (`Nemesis.CalmSince()` with a nemesis of your own) returns `ok` true when every nemesis fault has ended and buggify faults are off, and since when. Don't count faults yourself from `Event`; that bookkeeping went wrong in several harnesses.
- Clock skips move every node's wall clock, forward or back. `time.Since` and timers use the monotonic clock and ignore skips. Model a SUT that times expiry or ordering by wall clock with `time.Now().Round(0)`, and expect expiries and timeouts to be late or early by the skip.
- Syncs take time: fsync, fdatasync, `O_SYNC` writes and `msync(MS_SYNC)` block the calling goroutine from tens of µs to seconds while other goroutines and nodes run on, so a lease, election or write timeout can expire during a sync. The `sim.disk.slowsync` buggify site stalls a single sync for 10ms to 5s (timeline: `NODE: slow sync DUR`), and the nemesis's `sim.FaultDisk` slows one node's syncs 10 to 1000× or stalls them until the fault ends (`nemesis: start disk slowed NODE xN`, `nemesis: start disk stalled NODE`). Count a lease's term from when it was granted, not from after persisting it; `testdata/simlease` is a small example of this bug.
- Turn off a fault site only when the SUT documents that it does not survive that fault (for example `buggify.Disable("sim.disk.fsyncfail")` for a store that stops on fsync errors), and say why in a comment. Turning off faults to make failures go away hides bugs.

## Writing checks that don't raise false alarms

Each overnight project lost runs to its own oracle. The recurring causes:

- An operation that timed out, hit a crash, or returned an error such as EIO or "no space" may still have taken effect: etcd applies a write and then reports NOSPACE, clients retry behind your back. Track such operations as "maybe applied" and accept either outcome.
- A check's deadline must outlast the SUT's own backoffs and timeouts: client reconnect backoff, gRPC's 120 s reconnect backoff, TCP retransmission, election timeouts after a partition heals. Prefer "keeps making progress" over a fixed deadline for slow recoveries.
- Messages a fault held back arrive late, after the client gave up; a restarted node may receive requests sent to its previous incarnation.
- After a power failure, only what was synced survives, and recovery may legitimately roll back to any synced prefix.
- Once a known SUT bug wedges a node, liveness checks fail for the rest of the run; disarm them when the harness records that bug.
- A check that reads state the SUT updates concurrently must read it at a consistent point (after the operation it checks has returned, or from one snapshot).

## Before the first long search

- Plant a bug in the SUT (drop an fsync, acknowledge before writing) and check the harness catches it within a short search. A harness that catches nothing proves nothing.
- Count each error and timeout path you expect to exercise with `assert.Sometimes` or `assert.Reachable`, and check they fire. A missing piece of the simulation can make a feature unreachable without any failure.
- Run a short search (`detsched -duration 5m ./pkg`) after adding each workload feature, before a long one: a new feature's false alarms show up within minutes.
- Profile a few slow seeds with `perf record -g`: quadratic oracles and expensive SUT features (compression per message, big zeroed buffers that cgo allocators would have zeroed lazily) dominate cost, and slow runs reach the per-run timeout.
- To patch the SUT, use a local copy with a `replace` directive in go.mod; `-overlay` refuses files in the module cache.
