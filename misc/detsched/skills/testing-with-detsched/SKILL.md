---
name: testing-with-detsched
description: "Finds and root-causes concurrency and fault-tolerance bugs in Go code with detsched (GOEXPERIMENT=detsched): writing a workload or cluster harness with detsched's sdk/sim and sdk/compose, running `detsched ./pkg`, reading and triaging its failures (system under test, harness, or detsched itself), replaying a seed, and debugging the replay with dlv. Use when a task mentions detsched, GODETSEED, GODETBRANCH, synctest workloads under the experiment, sdk/sim, sdk/compose, or a failure printed by detsched. Not for ordinary go test failures or stock synctest tests."
---

# Testing with detsched

detsched is a Go toolchain fork. Under `GOEXPERIMENT=detsched`, a test that runs its goroutines in a `testing/synctest` bubble is fully determined by its seed, and virtual time never waits. `detsched ./pkg` searches seeds for failures and prints, for each kind of failure, a line that replays it exactly and one that replays it under dlv. Its defaults are meant to work: use no flags.

The reference is `$(go env GOROOT)/misc/detsched/README.md`, and debugging is `$(go env GOROOT)/misc/detsched/delve.md`.

## The loop

1. **Write a harness**: a Go test that runs the code in `synctest.Test` and checks it with `sdk/assert`, on simulated nodes with `sdk/sim` and `sdk/compose` when it uses the network or the disk. See `reference/harness.md`.
2. **Search**: run `detsched ./pkg` in the background, its output to a file. Its first line names `<outdir>/summary`, which it rewrites every 30 seconds; read that file instead of the progress. Each new kind of failure also prints a `FAIL` or `ALSO` block with its replay line as soon as it is found. Stop with SIGINT (or `-duration 10m`) when the groups stop changing; the summary is then final.
3. **Fix the fallbacks first.** A `WARNING: N fallbacks limited this search` block at the top lists each thing that limited the search or the replay lines, with what it costs, a one-line fix, and the README section that explains it. Fix those before triaging failures. The block also names what the runs missed: assertions never reached, `Sometimes` never true, runs that end before any fault, faults never injected; until it names none, a search without failures shows little. Then run `detsched -check ./pkg`, which plants one mistake at a time in the code under test and lists those no run caught.
4. **Triage each group** (`reference/triage.md`): read its log's first failure and the fault timeline at the end of the log; the `faults` and `also in N of these runs` lines say which faults it needs and what else failed in its runs, each with its own replay line. Classify it as the system under test (SUT), the harness, or detsched before fixing anything.
5. **Debug** with the group's `debug:` line, which runs dlv on a build without optimizations that replays the same run (`delve.md`).
6. **Fix, rebuild, and search again.** A rebuild gives every seed a new schedule, so judge a fix by a fresh search, never by an old seed.

## Rules

- Replay with the printed line exactly: its `cd`, every variable, the kept binary, its arguments. It works in a new shell and on another machine of the same OS and architecture, and `detsched` has checked that it reproduces; a replay that runs another schedule prints a warning that says what differs, such as a variable or the machine's CPU features. A replay reproduces when its `detsched: trace=` line equals the log's first one.
- Never add prints, `-test.v` or `-test.run` to a replay: any change to the binary, its arguments, or a variable it reads gives another run. Use dlv; diagnostics a harness prints must run only after an assertion failed (`assert.Failures() > 0`).
- Under dlv, keep `GODETBRANCH` from the line: dlv does not follow forks.
- Keep assertion messages constant, with values in the details: the message is the group.
- When you find a SUT bug, keep searching past it: put a workaround behind an environment flag (`KV_BUG=1` lets the bug happen), and confirm the bug by failure counts with and without the flag over the same `-n` runs.
- Package variables are shared by every simulated node and restart; read "Shared memory" in the README before writing a harness for a new system.

## Where to read next

- A harness: `reference/harness.md`, and `testdata/simcompose` in `$(go env GOROOT)/misc/detsched` as a complete example.
- A failure: `reference/triage.md`.
- The summary, the fallbacks, replay lines, flags, the simulation: the README.
- dlv: `delve.md`. The bundled dlv prints the run's position on every stop (virtual time, switch, tick, sim node), goes back (`checkpoint`, `restart c1`, `rewind`, `restart t9.4`), stops at the Nth hit of a hot function for free (`cond -hitcount`), and prints JSON (`--json`).
- The SDK's API: `go doc -all github.com/ryanworl/go-detsched/misc/detsched/sdk/sim`, and likewise `sdk/compose`, `sdk/assert`, `sdk/buggify`, `sdk/lifecycle`, in a module that requires the SDK.
