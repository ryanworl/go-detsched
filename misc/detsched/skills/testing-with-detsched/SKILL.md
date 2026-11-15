---
name: testing-with-detsched
description: "Finds and root-causes concurrency and fault-tolerance bugs in Go code with detsched (GOEXPERIMENT=detsched): writing a workload or cluster harness with detsched's sdk/sim and sdk/compose, running `detsched ./pkg`, reading and triaging its failures (system under test, harness, or detsched itself), replaying a seed, and debugging the replay with dlv. Use when a task mentions detsched, GODETSEED, GODETBRANCH, synctest workloads under the experiment, sdk/sim, sdk/compose, or a failure printed by detsched. Not for ordinary go test failures or stock synctest tests."
---

# Testing with detsched

detsched is a Go toolchain fork. Under `GOEXPERIMENT=detsched`, a test that uses `testing/synctest` runs one goroutine at a time, so a run is fully determined by its binary, its environment, and its arguments, and virtual time never waits. Package `github.com/ryanworl/go-detsched/misc/detsched/sdk/sim` runs the nodes of a distributed system in one process on a simulated network and disk with injected faults; package `.../misc/detsched/sdk/compose` drives a workload against them and checks it. `detsched ./pkg` searches seeds on every core, groups failures by signature, keeps each failing run's binary and log, and prints one replay line per failure.

The loop: write or change the harness, run `detsched ./pkg`, take each failure signature, replay it, classify it, fix the harness or record the bug, rebuild, and search again.

## Rules that cost hours when broken

- Replay with the printed line exactly: its `cd`, every variable on it (`GODETSEED`, `GODETBRANCH`, and `GODETSEARCH='...'` when present, quoted as printed), the kept binary, and its arguments. The line also sets the variables of `detsched`'s environment that the schedule depends on (such as `SIMCOMPOSE_BUG=1`), so it works in a new shell; `detsched` has already run it there and printed the verdict next to the failure (`the replay reproduced the failure in a new shell`, or what differed). Adding `-test.v` or `-test.run`, setting a variable the program reads, or rebuilding gives a different run. A replay reproduces when its `detsched: trace=` line equals the one in the run's log (for a branch failure, the log's first trace line, the one followed by `detsched: branch K`).
- If `detsched` prints `warning: seed 1 ran two different schedules`, the program depends on something besides its seed, such as the real clock read outside a bubble (for example `time.Now()` at package init). That warning is the only report of it, and no failure will replay reliably until it is fixed. Seed 1 runs twice at the start and once more when the search ends; the run at the end usually catches a clock read at init, so read the end of the output too. No warning doesn't prove the program deterministic; see `reference/triage.md`.
- A rebuild, even one that only adds a log line, gives every seed a new schedule. After changing the harness or the system under test, judge the change by a fresh `detsched` run, never by whether the old seed still fails.
- Never debug a replay by adding prints. Use dlv on the kept binary (see `reference/debugging.md`) and the fault timeline in the log. Diagnostics you add to a harness run only after an assertion has failed (`assert.Failures() > 0`); never gate them on an environment variable read on a path a passing run also takes, because reading it and branching on it moves the schedule.
- Under dlv, keep `GODETBRANCH` from the replay line. dlv does not follow forks; without it, a breakpoint inside a forked compose branch kills the branch with SIGTRAP (`branch N died of signal 5`).
- Classify every failure before fixing anything: system under test (SUT), harness, or detsched/sim. Most failures of a new harness are harness bugs. Read `reference/triage.md` before acting on a failure.
- When you find an SUT bug, keep the search going past it: put a workaround in the harness or a patched copy of the SUT behind an environment flag such as `KV_BUG=name` (unset: worked around; set: the bug can happen), then confirm the bug by comparing failure counts with and without the flag over the same seeds on the same binary.
- Package variables are shared by every simulated node and every restart of a node: global loggers, metrics and protobuf registries, `sync.Pool`s and channel pools, `sync.Once`. Read the "Shared state" part of detsched's README "Simulating a cluster" section before writing a harness for a new system.

## Where to read next

- Writing or changing a harness with sim and compose: `reference/harness.md`.
- A failure to read, replay, and classify: `reference/triage.md`.
- Stopping a replay in dlv, finding where two runs diverge, attaching to a hung run: `reference/debugging.md`. The full human guide is `misc/detsched/delve.md` in the detsched repository.

## Gotchas

- `t.Logf` output and `-test.v` are not available for diagnosis: `-test.v` changes the schedule, and a run that fails only through an assertion may never print the log. Write what you need to know (the configuration a run drew, the history a check depends on) into the harness's own failure dump.
- The fault timeline (`sim: 9.408758544 server: power failure`) is printed to stderr after a failing run's output. Its times are seconds since the World was created; read it before setting any breakpoint on a crash or disk fault.
- A breakpoint that never stops is not evidence that the code never runs. If `dlv.out` has `detsched: running again without ASLR`, dlv was started without `--disable-aslr` (needed on macOS) and lost every breakpoint. Otherwise check the location dlv reports when it sets the breakpoint, and confirm with a tracepoint on the function's entry.
- A hung run usually spins: some timer keeps moving the virtual clock. `detsched` stops it at its per-run timeout and the kept log holds every goroutine's stack; goroutines of crashed nodes are labeled `node N crashed` in their headers and are frozen, so read the live ones first.
- `detsched: memory is low, so the search keeps fewer stopped programs and starts fewer runs` and `N runs were stopped because memory ran out, not counted as failures` mean single runs grow large (single runs of a raft harness grew past 1.4 GB). Stopped runs count as neither pass nor failure, so the search loses what they would have found; bound what the harness keeps, such as its history and logs.
- `-test.cpuprofile` does not work under the experiment. Profile a slow harness with `perf record -g` on a replay.
- `go test -race` does not build under the experiment, and cgo is off: `detsched` builds with `CGO_ENABLED=0`.
