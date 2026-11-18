# Writing a harness with sim and compose

A harness is a Go test in its own package that runs the system under test (SUT) on simulated nodes, drives a workload against it while faults happen, and checks what the SUT promises. `misc/detsched/testdata/simcompose` in the release archive is a complete small example: read it before writing a new one.

## Contents

- Shape of a harness
- Nodes, goroutines, and shared state
- Locks and callbacks
- Faults and clocks
- Writing checks that don't raise false alarms
- Checks that find the bug, not only its symptom
- Before the first long search
- Comparing the SUT before and after a fix

## Shape of a harness

```go
func TestKV(t *testing.T) {
	if !sim.Supported() {
		t.Skip("requires GOEXPERIMENT=detsched")
	}
	synctest.Test(t, func(t *testing.T) {
		w := sim.NewWorld(sim.Config{
			OnPanic: func(n *sim.Node, msg string, stack []byte) { // a SUT panic crashes only its node
				dump() // the harness's diagnostics, which otherwise die with the program
				assert.Unreachable("node panicked", map[string]any{"node": n.Name(), "msg": msg})
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

- Set `sim.Config.OnPanic`. Without it, a panic in the SUT, which many systems raise on a broken invariant (raft's `log not found`), ends the program at once, before any diagnostics the harness prints after a failed check, and the log holds only the stack. A fault on unmapped memory goes to OnPanic as `fatal error: unexpected fault address 0x…` (since detsched17), but a fatal error the runtime can't attribute to user code, such as `concurrent map writes`, still ends the program without OnPanic and without the harness's dump: debug it from the stacks and fault timeline in the log, and a dlv replay.
- Create the World first thing in the bubble. The fault timeline's times are seconds since the World was created, and dlv's `runtime.curg.bubble.now` lines up with them only when the bubble and the World start together.
- Draw everything random from the seed: `sim.Intn`, `sim.Rand`, `math/rand`, or map order all come from `GODETSEED`. Draw configuration per run (cluster size, SUT options, timeouts) so the search explores it, and record what was drawn in the harness's failure dump.
- Terminal checks (`Eventually`, `Finally`) also run on branches of the timeline: compose forks the process at random points, the branch runs the check, and the timeline goes on. A failure there names its branch, which `GODETBRANCH` replays. A branch returns from `c.Run` after its check and runs the rest of the test body, as the timeline does at its end, so code after `c.Run`, such as a final check or a dump, runs in every branch.
- `Eventually` kills the drivers by killing the client node they run on. A driver that was in the middle of restarting a SUT node (Stop then Start) stops halfway, and the check sees a node that is up without its server. Restart SUT nodes from the harness's own loop or from the nemesis's `Restart`, not from a driver.
- `x.Until(timeout, f)` returns early when the drivers' context ends, with f's last error. Check its result.
- Drivers that compose killed keep their goroutines frozen, and goroutines a driver started (an RPC past its timeout) may keep running. Give harness state a generation number when a late writer could corrupt it.

## Nodes, goroutines, and shared state

- Code that uses the network or the disk must run on a node: in `Node.Start(f)`, in goroutines `f` starts, or in `Node.Go(f)`. The test body and its goroutines belong to no node; their socket calls fail with `EPERM`. Compose runs drivers and checks on its client node (`x.Node()`).
- Nodes have no name resolution: dial IP addresses.
- Read a SUT node's in-memory state from that node, with `n.Go(f)` and a timeout, not from the test body. A goroutine of no node that blocks on a lock a crashed node held never wakes.
- Goroutines are still preempted anywhere. Harness maps and slices shared by goroutines need a mutex, or the run dies with `concurrent map writes`.
- Join every goroutine the harness starts before the test body returns, or synctest reports a deadlock.
- A restart tests only what the SUT keeps across it. A library that ships only in-memory stores, as raft libraries often do for their log and stable stores, loses everything at a restart unless the harness keeps them. Keep each node's store objects in the harness, outside the node, and hand them to the restarted node: that models a disk that never loses an acknowledged write, which is enough for protocol bugs such as a vote or term the node forgets, and takes minutes. A small store over files in the node's directory, with fsync where the SUT expects durability, also lets power failures, failed fsyncs and bad sectors reach the SUT; write it when the bug may be in recovery. Either way, a SUT that keeps its state only in memory finds no bug that a restart exposes. Prefer a store the SUT ships for disk, on the node's simulated files, wherever one exists (such as a file-based snapshot store): it is written to be crash-safe. An in-memory store written for unit tests is not, and kept across a crash it fails in ways a disk does not: it may publish an object before its writer finished it (a snapshot listed at `Create`, before `Close`), or break on states unit tests never reach (deleting a range below its first index). When a failure starts in such a store, check the store before the SUT.
- Package variables of the SUT and its dependencies are shared by every node and every restart: the README's "Shared state" bullet in "Simulating a cluster" lists the common ones and the fixes (an instance per node, global state registered once before starting nodes, per-process state keyed on `os.Getpid()`, which differs between nodes and between restarts of one node). A SUT that registers a global per server (one HTTP pool, one metrics registry) needs a small patch.
- A package that captures `time.Now()` in a package variable at init, before the bubble, and later compares bubble time with it makes runs differ from each other. Nothing reports the read itself: it shows up only as `detsched`'s warning that seed 1 ran two different schedules, and as replays that don't reproduce. Take such values inside the bubble, or patch the SUT.
- A dependency that starts a ticker or timer at init, outside the bubble, such as `go.opencensus.io/stats/view`'s worker with its 10-second ticker, wakes a goroutine when real time says, so runs part where it fires. The runtime reports the first firing, and every one under `dettrace=2`, as `a timer outside any synctest bubble fired …, at sN kN`; a slow run, under `dettrace=2` or a debugger, makes it fire earlier in the run. Stop the ticker or set its period before anything slow runs, or patch it out.
- A dependency that starts a subprocess with `os/exec`, even at init, makes the schedule depend on how fast the OS runs it. The runtime reports the start with the goroutine's stack, and `detsched` prints the reports of seed 1 as `warning: seed 1 left the deterministic subset` when the search starts. Keep the tool off `PATH` or patch the call out.
- A cleanup or finalizer that stops a timer created in the bubble kills the run (`stop of synctest timer from outside bubble`), as in stock synctest.

## Locks and callbacks

- Never hold a harness lock across a call into the SUT, and never share one harness lock between nodes: when the nemesis pauses a node whose goroutine holds it, every other node that takes it stalls until the pause ends, which reads as a liveness bug in the SUT. Keep locks per node, or use atomics or lock-free rings. For a check across nodes, such as one leader per term, give each node its own recorder, a slice under that node's own mutex or an `atomic.Pointer` to an immutable copy, and have the check read every node's: a paused node then delays only the check's read of that node, and with `atomic.Pointer` not even that.
- The nemesis calls `Restart` and `Event` on its own goroutine, which alone resumes paused nodes. They must return promptly and must not take any lock a node's goroutine can hold, nor wait for a node. The runtime fails the run with the callback's stack when one does.
- Don't log from nodes through a shared `log.Logger` or a library's global logger: a node that crashes while holding the logger's mutex wedges every node that logs. Give each node its own logger, or write to a lock-free ring.
- Stamp log lines with `w.Elapsed()`, the fault timeline's time (for hclog, `TimeFn`), not the wall clock, which clock skips move by minutes either way, so that node logs line up with the timeline.

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

## Checks that find the bug, not only its symptom

- Assert the protocol's own invariants where the SUT breaks them, not only what a user would see: for a consensus system, at most one leader per term (from an observer or the SUT's state) next to "replicas apply the same commands". A broken invariant shows in every run that breaks it; its symptom, such as two replicas diverging, shows only in the few runs where the damage then reaches a client. In one evaluation, the leader check failed 10 to 100 times as often as the divergence check, and pointed straight at the cause.
- Give each invariant its own assertion message, and put the cause in a liveness check's message ("no leader for 30s" rather than "converges"). `detsched` groups failing runs by the message of their first failed assertion, without its details, but when the details are only an error, as a compose check reports its failure, by the error's first line too, numbers aside: a check whose errors start with what failed ("two leaders in term 7", "lost write k3") makes a group of each. The summary also lists, under each group, the other failures its runs had, such as the symptoms of the group's failure.
- Expect the SUT's own liveness bugs and the harness's mistakes to fail first: in an evaluation on a raft library, the first searches failed on snapshot and log-compaction liveness bugs unrelated to the bug sought. Triage each group, fix the harness or put the SUT's known bug behind a flag, and search again. To keep counting a known liveness problem without failing runs on it, report it with `assert.Sometimes(ok, msg, details)` instead of failing: the summary says in how many runs it held, and a check that never held shows as `NEVER TRUE`.
- A failing `Anytime` check runs no more in that timeline. Print the harness's diagnostics once per run, not per failure: a dump on every failure makes logs of hundreds of megabytes.

## Before the first long search

- Plant a bug in the SUT (drop an fsync, acknowledge before writing) and check the harness catches it within a short search. A harness that catches nothing proves nothing.
- Count each error and timeout path you expect to exercise with `assert.Sometimes` or `assert.Reachable`, and check they fire: the search's summary lists each with the runs it held in, `NEVER TRUE` first. An assertion no run reached is not listed, so prefer `Sometimes(cond)` at a place every run passes. A missing piece of the simulation can make a feature unreachable without any failure.
- Run a short search (`detsched -duration 5m ./pkg`) after adding each workload feature, before a long one: a new feature's false alarms show up within minutes.
- Time one seed's replay before a long search, and again after the fix: a run costs the code its goroutines run, the harness's checker included, so a checker that hashes or compares every value byte by byte, or scans the whole store after each operation, makes runs take tens of seconds. The summary prints the slowest passing run's replay line when it took 5 seconds or more.
- Profile a few slow seeds with `perf record -g`: quadratic oracles and expensive SUT features (compression per message, big zeroed buffers that cgo allocators would have zeroed lazily) dominate cost, and slow runs reach the per-run timeout.
- To patch the SUT, use a local copy with a `replace` directive in go.mod; `-overlay` refuses files in the module cache.

## Comparing the SUT before and after a fix

Build one test binary from each version of the SUT and search each with the same number of runs. `detsched` builds with the experiment and without cgo; do the same:

```
cd /abs/harness
GOFLAGS=-modfile=go.before.mod GOEXPERIMENT=detsched CGO_ENABLED=0 go test -c -o before.test .
GOEXPERIMENT=detsched CGO_ENABLED=0 go test -c -o after.test .
detsched -exec ./before.test -n 3000
detsched -exec ./after.test -n 3000
```

Here `go.before.mod` is a copy of go.mod whose `replace` points at an unchanged copy of the SUT, next to a copy of go.sum named `go.before.sum`, which `-modfile` reads instead; editing the SUT in place and rebuilding works too, but then check which version a binary holds before searching it. `detsched -exec` runs a copy of the binary it keeps with the logs, and replay lines name the copy, so rebuilding `before.test` later changes neither. Report failures over runs for both. A run of the fixed SUT goes on where the buggy one failed, so the same `-n` can take several times as long after the fix. A fix changes the schedule, and the guided search's runs differ between the two binaries, so compare rates, not seeds. A flag the harness reads (an environment variable that turns a workaround on) works the same way with one binary.
