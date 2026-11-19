# detsched

`GOEXPERIMENT=detsched` makes Go programs that run their concurrency inside
`testing/synctest` bubbles deterministic: one seed gives one schedule, every
time, and bubbles never wait in real time. The `detsched` command searches a
package's tests for schedules (and, with [`sdk/sim`](#simulating-a-cluster),
faults) in which they fail, and prints a command that replays each failure
exactly, under a debugger too.

This is a fork of Go 1.27.4 for testing only: all randomness, including
`crypto/rand`, comes from the seed. It runs on Linux (amd64, arm64) and
macOS on arm64.

## Getting started

Download a release for your platform from
https://github.com/ryanworl/go-detsched/releases and put its `bin`, which
has `go`, `detsched` and Delve's `dlv`, on your `PATH`:

```
v=go1.27.4-detsched25
mkdir -p ~/sdk
curl -fsSL https://github.com/ryanworl/go-detsched/releases/download/$v/$v.linux-amd64.tar.gz | tar -C ~/sdk -xz
export PATH=$HOME/sdk/$v/bin:$PATH
```

Use `linux-arm64` or `darwin-arm64` for those platforms. The macOS
binaries are not notarized: download them with curl, or run
`xattr -d com.apple.quarantine` on an archive a browser downloaded. To
build from this tree instead, run `misc/release/build.bash` and put
`$PWD/bin` on your `PATH`.

Then run it on a package whose tests use `testing/synctest`:

```
detsched ./pkg
```

It builds the package's test binary with the experiment, runs it on every
core but one, one seed after another, until you stop it (Ctrl-C, SIGINT or
SIGTERM), `-duration` passes, or `-n` runs have run, and then prints the
summary. While it runs, it prints a status line (on a terminal, updated in
place; otherwise every 10 seconds), a `FAIL` block for each new kind of
failure once its replay has been checked, and an `ALSO` block for each
other kind of failure found in a run after its first one. It rewrites the
summary so far every 30 seconds in a file whose path it prints first.

```
$ detsched ./testdata/bugcache
detsched: the summary so far is in /tmp/detsched-334332647/summary, every 30 seconds
FAIL panic: computed answer twice [recovered, repanicked]
	log:    /tmp/detsched-334332647/fail-1-1.log
	replay: cd /home/me/go/misc/detsched/testdata/bugcache && GODETSEED=1 GODETBRANCH=0 GODETSEARCH=trace=0x5a28688c3de0741d /tmp/detsched-334332647/bugcache.test
	debug:  cd /home/me/go/misc/detsched/testdata/bugcache && GODETSEED=1 GODETBRANCH=0 GODETSEARCH=trace=0x5a28688c3de0741d dlv exec --disable-aslr /tmp/detsched-334332647/debug/bugcache.test
	the replay reproduced the failure in a new shell
^C
detsched: ./testdata/bugcache: 489 runs, 174 failed, 1 signatures, 0 features, 736.0 runs/s, 1s
...
FAIL 174 of 489 runs: panic: computed answer twice [recovered, repanicked]
	log:    /tmp/detsched-334332647/fail-1-1.log (and 9 more of these runs: /tmp/detsched-334332647/fail-1-*.log)
	replay: ...
	debug:  ...
```

The search keeps its binaries, logs and summary in a new directory under
`$TMPDIR` (else `/tmp`); a search that finds no failure removes all but the
summary and the logs and binary the summary names. `detsched` exits with
status 1 if any run failed.

## Reading the summary

The summary starts with the counts: runs, failed runs, signatures (groups
of failures), features (the new states the search reached, see
[the search](#the-search), which stop growing once it has found most of
what it can reach), runs per second, and time.

Then, if anything limited the search, a `WARNING` block lists each
[fallback](#fallbacks): what happened, what it costs, how to fix it, and
the section of this file that explains it. Fix those first: a search
with fallbacks finds less, or prints lines that may not replay.

Then the statistics:

- how runs started (fresh seeds, forks of cells, runs directed at a
  predicted deadlock), and what counts as a feature;
- the variables of `detsched`'s environment that the schedule depends on,
  which replay lines set;
- lock orders that predict a deadlock, with the runs directed at them and
  how many failed (a prediction no directed run fails is likely a false
  alarm: locks are told apart by address, and a freed lock's memory may
  be reused for another, as happens with `context` cancellations);
- the slowest passing and failing runs, to choose `-timeout` from, and the
  replay line of the slowest passing run when it took 5 seconds or more;
- the program's `assert.Sometimes` and `assert.Reachable` assertions,
  never-true ones first, with the runs each was reached and held in (one
  no run reached is not listed), the runs that evaluated each
  `assert.Always`, which a search with no failures shows its checks ran
  in, and on one line each, the runs that
  reached each fault the simulation injected (`fault:`), each nemesis
  fault, each compose feature, and each kind of node panic;
- when runs failed more than one way, the runs that failed each way, first
  or after another failure.

Then one block per group:

```
FAIL 82 of 6848 runs: detsched: assertion failed: acknowledged write survives
	its first run: map[acked:199 got:[198]]
	faults in its runs: sim.disk.fsyncfail in 82 of 82 (26% of passing runs)
	log:    /tmp/detsched-2884579083/fail-1-1.log (and 9 more of these runs: ...)
	replay: cd ... && GODETSEED=4 GODETBRANCH=0 GODETSEARCH='tape=... trace=0x...' /tmp/.../store.test
	debug:  cd ... && GODETSEED=4 GODETBRANCH=0 GODETSEARCH='tape=... trace=0x...' dlv exec --disable-aslr /tmp/.../debug/store.test
	also in 3 of these runs: detsched: assertion failed: reopen succeeds ... (first in fail-1-4.log)
		its replay: cd ...
```

- The heading is the group's signature: its runs' first assertion failure
  ([`sdk/assert`](#the-sdk)), panic, or fatal error, else their first
  failing test, with numbers masked and an assertion's details left out,
  except that when the only detail is `err`, as a compose check reports
  its failure, the first line of the error is part of it. The next line
  gives the first run's details, which other runs need not share.
- `faults in its runs` names the simulation's faults and nemesis faults
  that fired in at least half of the group's runs, and in a share at
  least 25 points above their share of passing runs: the faults the
  failure likely needs.
- For a deadlock at the end of a test (`blocked goroutines remain`), a
  line names the goroutines left, by where they wait and what created
  them.
- `detsched` keeps the logs of the group's first ten runs, as
  `fail-G-1.log` and on, and of the first run with each other kind of
  failure. A log starts with the run's replay line and its failures, each
  after `# `, then the run's whole output, its first and last 8 MB if
  longer. A failure inside a branch of a [composer](#simulating-a-cluster)
  timeline is that branch's failure: its log starts with the branch's
  output and fault timeline, then the timeline's.
- `also in N of these runs` lists the other kinds of failure the group's
  runs had, such as symptoms of the first, or another bug, with the replay
  line of the first run that had it when that differs from the group's.
  So a frequent bug does not hide a rarer one: debug the rarer one from
  its replay line without fixing the first.
- A line `WARNING: the replay ...` says that running the replay line in a
  new shell did not reproduce the failure. When every group's did, one
  line before the groups says so.

## Fallbacks

Each entry of the summary's `WARNING` block has a section here.

### Seed 1 ran two schedules

Seed 1 runs twice at the start, once more a minute in, and once more when
the search ends. Different trace lines mean the program depends on
something besides its seed, usually the real clock read outside a bubble
(`time.Now()` in a package variable at init, later compared with bubble
time), and failures may not replay. Nothing else reports such a read, and
a program that passes this check is not proven deterministic: a replay
that does not reproduce is the other sign. Fix it before triaging: build
the binary as `detsched` does
(`GOEXPERIMENT=detsched CGO_ENABLED=0 go test -c -o pkg.test`), run
`GODETSEED=1 GODETBRANCH=0 ./pkg.test` twice with `GODEBUG=dettrace=2`,
and see what the goroutine at the first difference was doing
([delve.md](delve.md#find-where-two-runs-diverge)).

### Seed 1 left the deterministic subset

The runtime reports, once per stack, each place where a program depends on
the OS or on real time: waiting for a real file or network connection to
become ready, staying in a system call for 10 ms of real time, starting a
subprocess, a timer outside every bubble firing, and a sleep outside every
bubble ending. Each report says where in the run it happened (`at sN kN`,
the switch and tick `GODETSTOP` takes) and prints the goroutine's stack;
`GODEBUG=dettrace=2` makes it every time. `detsched` writes seed 1's
output, with the stacks, to `seed-1.log`. Move that work into the bubble
or onto a sim node: a dependency's subprocess (keep the tool off `PATH`
or patch the call out), or a ticker it starts at init (stop it, or set
its period, before anything slow runs). The runtime does not catch system
calls that return data from the OS, such as the process ID or file
contents.

### ASLR

On macOS, a program that finds itself loaded at a random address runs
itself again without ASLR (see [macOS](#macos)). When that fails, it
prints `detsched: cannot run again without ASLR: ...` and its schedule may
depend on where it is loaded. A set-user-ID program never does it.

### New shell

`detsched` runs seed 1 in a new shell's environment too. When the schedule
differs there, it looks for the variables of its own environment that the
schedule depends on and sets them on replay lines (such as a harness flag
`SIMCOMPOSE_BUG=1`, or `NO_COLOR`, which logging libraries read at init).
This entry means it could not find them: run replay lines from the shell
that ran `detsched`.

### Fork refused

The search forks stopped programs, so that fresh seeds skip setup and runs
start from interesting states. A program refuses to fork while
`runtime.iscgo` is set (by cgo, which `detsched`'s `CGO_ENABLED=0` build
leaves off, or by `github.com/ebitengine/purego/internal/fakecgo`), while
a goroutine is in a system call or locked to its thread, while a file is
registered with the network poller (real pipes and sockets), once it has
used the real file system through package `os` (a regular file open, or
any file or directory created, such as `t.TempDir()`), since forks would
share the files, and on macOS once it has used the system's certificate
verifier. A refusal for cgo or the file system stops all forking: the
search runs fresh seeds only, several times fewer runs per second for a
program with setup, and no cells or directed runs, which find bugs that
need several rare events in a row. Other refusals cost one launch each,
which runs from the start instead. The entry says the reason and the fix:
put files on a sim node's disk, close real sockets or move them onto sim
nodes, give TLS clients a fixed root pool (`tls.Config.RootCAs`), or drop
the dependency that imports `fakecgo` (`go list -deps` finds it).

### No faults

The simulation injects none of its faults (failed fsyncs, bad sectors, a
full disk, slow syncs, retransmissions, resets, UDP faults) until setup
completes, as Antithesis injects none before it, so that opening a store
does not fail on an injected error. Setup completes at the first of:
`lifecycle.SetupComplete`, `World.SetCalm(false)` (which compose calls
when its `First` commands end), `Nemesis.Start`, and the first fsync of a
file on any node, which ends a store's first open. This entry means no
run got there: call `lifecycle.SetupComplete(nil)` once setup is done.
The fault timeline says when it happened (`world: setup complete: faults
on`).

### Branch did not run

A composer timeline forks a branch for each terminal check. Where the
process cannot fork (see [Fork refused](#fork-refused)), the runtime prints
`detsched: branch K did not run: the process cannot fork now (REASON)`,
the branch's check does not run, and the branch keeps its number, so
`GODETBRANCH` still counts the branches after it right. The fix is the
reason's.

### Replay did not reproduce

`detsched` runs each group's replay line in a new shell. When it ends
another way, or fails the same way on another schedule (another trace
line), the line may show another run than the failure, and debugging it
misleads. A rerun that finds the variables the run depended on fixes the
line itself. Otherwise the program depends on something the line cannot
carry: see [Seed 1 ran two schedules](#seed-1-ran-two-schedules).

### Replay not checked

The search ended before it could run a group's replay line. Run it, and
check that its trace line equals the first one in the log.

### Debug lines run the optimized binary

A build of the same source with `-gcflags='all=-N -l'` runs the same
schedule, so dlv reads every local in it. That holds for a program that
reads its build info, as libraries do to learn their version:
`debug.ReadBuildInfo` leaves out the `-gcflags` setting in det mode, so
both builds see the same settings (`go version -m` still prints it).
It holds too for the caches that reflection-heavy packages such as
`encoding/gob` and `encoding/json` keep in a `sync.Map` keyed by types,
and for `reflect.ArrayOf`, `SliceOf`, `PointerTo` and the like: the
hash tries behind `sync.Map` and `unique`, and reflect's search of the
binary's types, count no ticks, because how much work they do depends on
addresses and on what types the binary holds, which differ between the
builds. And it holds for maps keyed by pointers or channels, or by
structs, arrays or interfaces that hold them, including `sync.Map` and
`unique`: such a key hashes by its pointer's identity, not its address
(see [How the runtime makes runs deterministic](#how-the-runtime-makes-runs-deterministic)),
so `range` visits it in the same order in both builds. `detsched` makes this build in `<outdir>/debug/` while it searches, runs
seed 1 in it, and from then on prints `debug:` lines that run it. They run the optimized binary instead
when the build was not ready when the search ended, with `-exec`, or when
seed 1 ran another schedule in it three times, because the program depends on heap
addresses (printing pointers, hashing or sorting them as numbers), its own stack
(`runtime.Stack`, `runtime.Callers`, a node's `Stacks`), or memory
statistics (`runtime.ReadMemStats`, GC counts). In the last case debug the
optimized binary; otherwise build one yourself
([delve.md](delve.md#read-locals-replay-a-build-without-optimizations)).

### Memory

While less than a quarter of the memory (of the machine, or of its
cgroup) is available, `detsched` closes its stopped programs and starts no
runs; below a tenth, it stops the run holding the most private memory,
with `SIGQUIT`, so that its log (`memory.log`, for the largest) shows
where it was, and counts it as neither pass nor failure. A seed 1 stopped
this way ends the search with its replay line. A run that grows without
bound is a bug of the harness or the program, such as a history it
keeps forever; bound it, or lower `-p`. On Linux the replay line of the
largest runs it in a subshell with `ulimit -v` set above the size it was
stopped at, so that it ends with `fatal error: out of memory` and its
stacks instead of exhausting the machine.

### Switch limit

A forked run that makes 1000 times the goroutine switches of a whole run
that passed (counted since its last fork) is taken to spin, usually a
loop in virtual time such as a retry without a backoff or a timer that
keeps firing, and is stopped, not counted as a failure. `detsched` keeps
the first one's log (`spin.log`) and replays it: its replay line carries
the same limit (`maxswitches=` in `GODETSEARCH`), so it stops at the same
switch with the same trace line. If the run loops forever, it is a
liveness bug.

### Still running

When a search is cut short (`-duration`, a signal), runs going on for over
10 times as long as the slowest passing run (and at least 10 seconds),
which the status line counts while it runs, are stopped with `SIGQUIT`
and grouped as `still running when the search ended`, with their stacks
in their logs. A replay that ends was only slow; one that does not hangs.
Shorter runs are dropped. Runs that pass `-timeout` are stopped the same
way and grouped as `timeout`.

### Output cut

A run keeps at most 16 MB of its output, its first and last 8 MB, with a
line where the rest was cut. Print diagnostics once per run, after an
assertion failed (`assert.Failures() > 0`), not on every failure.

## Replaying a failure

Run the printed replay line as it is: its `cd`, every variable on it, the
kept binary, and its arguments. It sets:

- `GODETSEED`, the seed, which sets all randomness;
- `GODETBRANCH`, the composer branch the failure was in, run in the process
  itself rather than in a forked child, or 0 to run the timeline without
  forking any branch;
- `GODETSEARCH`, quoted, for a run forked from another (`tape=`, its fork
  points), directed at a deadlock (`yield=`), or stopped at the switch
  limit (`maxswitches=`), and the run's schedule hash (`trace=`): a replay
  whose schedule differs says so at exit
  (`detsched: warning: this run's schedule is not the one its replay line
  was printed for`);
- the variables of `detsched`'s environment the schedule depends on, and
  every other variable the program read, as the run had them, and unsets
  with `env -u` those it read that the run did not have (`TZ`, which
  package `time` reads, is the usual one), so the line runs the same in
  any shell.

The run prints `detsched: trace=0x... switches=N syncs=M` at exit (a hash
of the schedule, the goroutine switches, and the synchronization points
passed, which a busy run counts in the hundreds of millions); the replay
reproduces when that line equals the one in the log (for a branch failure,
the log's first one, followed by `detsched: branch K`). The lines after it
are for `detsched`, not for reading: `perturb` is the scheduling strategy
the seed drew, `busy` the CPU time charged to nodes, `environment read`
the variables the program read, which replay lines set, and `feedback`
and `assert` lines the features and assertion counts the search uses.

Change nothing. Another binary (but the one without optimizations below),
other arguments (even `-test.v` or `-test.run`), another value of a
variable the program reads, the name of another `GODEBUG` key, or a line
of output added to the program, which is a rebuild, gives another
schedule. Code that prints diagnostics must run only after an assertion
has failed (`assert.Failures() > 0`), never behind a variable a passing
run also reads. Breakpoints, time spent stopped, `GODEBUG=dettrace=2`,
`GODETSTOP`, `GOTRACEBACK`, attaching a debugger, and copying the binary
leave the run unchanged; add such a variable just before `GODETSEED=`,
after `env -u NAME`.

The `debug:` line is the replay line with `dlv exec --disable-aslr` before
the binary, which it replaces with the build without optimizations once
that ran seed 1's schedule (see
[Debug lines run the optimized binary](#debug-lines-run-the-optimized-binary)).
`--disable-aslr` keeps dlv's breakpoints on macOS and does nothing on
Linux. Keep `GODETBRANCH`: dlv does not follow forks, and a breakpoint in a
forked branch kills it (`branch K died of signal 5`). The bundled dlv
knows it debugs a replay: every stop and tracepoint line ends with the
run's position (`at t25.380001899 s4265 k128779 node server (start 3)`:
virtual time, switch, tick, sim node), `goroutines -with node server`
lists a node's goroutines, `checkpoint`, `restart c1`, `rewind` and
`restart t9.4` go back by running the replay again, `cond -hitcount NAME
== N` stops at the Nth hit of a hot function at no cost for the others,
`--json` prints one JSON object per line, and `exit` lets the program
print its trace line. [delve.md](delve.md) shows how.

Three variables help without a debugger, and change nothing:
`GODEBUG=dettrace=2` prints every goroutine switch and every jump of a
bubble's clock, to diff two runs; `GODETSTOP=t9.4,s12345,k812345678`
stops at points in virtual time, switches, or ticks (one per entry of a
function that calls or loops, and per loop iteration) by calling
`runtime.detStop`, where a debugger can break; and `GODETSTOP=t9.4:dump`
prints every goroutine's stack there and runs on.

## Flags

All are optional.

- `-run regexp` runs only the matching tests, as `go test -run` does.
- `-duration d` stops after `d`; `-n N` stops after N runs, so that a CI job
  checks the same runs every time. Either then runs seed 1 once more
  before the summary; a signal skips that run.
- `-timeout d` (default 10m) stops a run that takes longer, with
  `SIGQUIT`, so that its log has every goroutine's stack, and counts it as
  a failure.
- `-p n` (default: one fewer than the CPUs) runs `n` programs at once.
  Lower it when runs take enough memory to make the machine swap.
  `detsched` and its runs run at nice 10, so a search yields the CPUs to
  the rest of the machine's work.
- `-seed-base n` adds `n` to every seed after seed 1, so that machines
  splitting a search run disjoint seeds: shard `i` of a search with
  `-seed-base $((i << 32))` runs seeds `i<<32+2`, `i<<32+3`, and on.
- `-exec binary` runs a prebuilt binary in the current directory instead
  of building a package, so that one job builds it and every shard runs
  the same file, or two versions of the code are compared by their
  failure rates over the same `-n`. `detsched` runs a copy it keeps, which
  replay lines name. Build it as `detsched` does, in the package's
  directory: `GOEXPERIMENT=detsched CGO_ENABLED=0 go test -c -o pkg.test`.

## The search

Each seed runs another schedule: the runtime perturbs the order in which
goroutines run, choosing for each seed between a random walk and PCT
(probabilistic concurrency testing) and their parameters. If the program
reports the end of its setup (`lifecycle.SetupComplete`), `detsched` runs
setup once in each of a few seeds and forks the stopped processes, each
fork reseeded right after setup, so fresh seeds don't pay for setup. On
top of fresh seeds:

- A run that reaches a state no run reached before makes it a cell. A
  cell is a run's seed, fork points, and the switch where it got there; to
  explore it, `detsched` runs it to that switch, keeps the process stopped,
  and forks runs from it with new random choices, so a failure that needs
  several rare events in a row is found one event at a time. A state is
  new when the program reports a new `guide.State` or assertion outcome,
  if it reports guide states at all, else when the runtime records a new
  place where goroutines park, a new `select` case, a new lock order, a
  recovered panic, or a new maximum of goroutines, channel buffers, or
  virtual time. A run makes at most one cell, at its first new state.
- When a run takes two mutexes in the order opposite to another
  goroutine's, the lock order predicts a deadlock, and some runs then make
  goroutines that take the first of those mutexes yield while they hold
  it, so that the other goroutine can take the second.

`detsched` spends its runs on fresh seeds, cells, and directed runs in
proportion to the new cells and failures each produced lately per second,
and gives each at least a tenth of the runs; it picks cells the same way.

## The SDK

Packages under `sdk` let a test tell `detsched` what matters. They are the
module `github.com/ryanworl/go-detsched/misc/detsched` in this directory,
and release N's version is v0.N.0, so with go1.27.4-detsched25:

```
go get github.com/ryanworl/go-detsched/misc/detsched@v0.25.0
```

Without the experiment they do nothing, or print, so tests build and pass
with stock Go. `go doc -all` on each documents its API.

- `assert`, after the Antithesis SDK: `Always`, `AlwaysOrUnreachable`,
  `Sometimes`, `Reachable`, and `Unreachable`, identified by their
  message, so keep messages constant and put values in the details. Each
  failing assertion is reported once at exit, as
  `detsched: assertion failed: MESSAGE DETAILS (and N more failures)`,
  with its first failure's details formatted as `map[k:v ...]`, sorted by
  key, a value with a space quoted, then where the first one failed,
  with the `GODETSTOP=kN:dump` that prints every goroutine's stack there;
  a breakpoint on `runtime.detAssertFailed` stops at every failure. A
  test during which one failed fails too, so that its log is printed, and
  a run that would exit with status 0 exits with status 3.
  `assert.Failures` counts the failures so far.
- `buggify`, after FoundationDB's BUGGIFY: `buggify.On("site")` marks a
  place to inject a fault. Each run enables a quarter of the sites (swarm
  testing), and an enabled site fires a quarter of the time. `Disable`
  turns sites off, `Enable` allows opt-in ones, and `Force` makes one
  fire every time.
- `guide`: `guide.State(name, s)` reports an abstract state of the
  program, such as each node's role, and `Maximize` a value to push up.
  When a program reports states, only they and assertions make a run new.
- `lifecycle`: `SetupComplete` marks the end of setup, which the search
  then runs once per seed rather than once per run, and after which the
  simulation injects faults; `Branches` tells a test with a fixed-length
  workload to count it from the last fork.
- `sim` and `compose` simulate a cluster; see below.

## Simulating a cluster

Package `sdk/sim` runs the nodes of a distributed system in one process,
each with its own simulated network stack and disk, so that unmodified
code using `net` (TCP and UDP over IPv4) and `os` talks over a simulated
network and stores data on a simulated disk. Create a `World` inside a
synctest bubble, add nodes with `AddNode(name, ip)`, and start each with
`Start(f)`: `f` runs in a goroutine that belongs to the node, and so do
the goroutines it starts, also through `time.AfterFunc`. On those
goroutines, path and socket system calls go to the simulation, which also
owns the descriptors it returns, on any goroutine. Other goroutines use
the real OS. A harness needs little more than that, and a `Nemesis` (or
the composer, below) to inject faults:

```go
w := sim.NewWorld(sim.Config{})
defer w.Close()
n1 := w.AddNode("n1", "10.0.0.1")
n1.Start(serve)
nm := w.NewNemesis(sim.NemesisConfig{
	Restart: func(n *sim.Node) { n.Start(serve) },
})
nm.Start()
// ... drive the cluster and check it ...
nm.Stop()
```

A run that fails prints the fault timeline after its output: every start,
crash, stop, pause, disk fault, clock skip, and nemesis fault, and the
first failure of each assertion while the world exists, under the node
whose goroutine failed it (`world` for no node), with the simulated time
since the World was created, such as `sim: 12.004000000 n1: power
failure`. A passing run prints none of it, unless `GODEBUG=dettimeline=1`
is set, as on a passing seed's replay line to check what the harness did.

- **Network.** Every write becomes segments that arrive after a latency
  drawn from the fault stream, in order on their connection. Links have a
  latency of 1ms plus an exponential jitter with a mean of 1ms; `SetLink`
  changes a directed link (latency, jitter, MTU, and a drop rate, where a
  lost packet shows up as a TCP retransmission delay). Half of the worlds
  round arrivals up to whole milliseconds, so that messages and timers
  coincide and the scheduler decides their order. Network faults act on
  every segment, also on established connections, in one of three modes:
  `Stopped` (held until the fault ends, or the connection fails with
  `ETIMEDOUT` after 30s), `Slowed` (added latency with a mean and
  deviation), or `Jammed` (held, then delivered, never timing out).
  `Partition`, `Isolate`, and `Block` stop traffic between groups, around
  a node, or on one link; `PartitionGroups`, `Clog`, and `Cut` do the same
  in any mode. Each returns a `NetFault` to `Heal`, and `World.Heal` ends
  them all. A connection attempt that cannot get through retries for 20s.
  TCP has Linux's windows (4 MiB in flight, 6 MiB unread) and keepalive.
  Deadlines and timeouts use the bubble's clock. Nodes have no resolver:
  they dial IP addresses.
- **UDP.** Each datagram travels on its own, so datagrams overtake each
  other, and is lost with the link's drop rate, beyond the link's MTU,
  across a `Stopped` fault, at a node that is down or a port nothing is
  bound to, or when the receiver's buffer is full; a `Jammed` fault holds
  datagrams until it ends. Sockets support what QUIC stacks use on Linux:
  `sendmsg`, `recvmsg`, `sendmmsg`, `recvmmsg`, the ECN bits of the TOS
  byte, `IP_PKTINFO`, and segmentation offload (`UDP_SEGMENT`).
  `Node.SetNAT` puts a node behind a NAT, which `Node.Rebind` rebinds.
- **Disk.** Writes stay in a page cache until fsync. With an ordered
  journal's semantics, fsync of any file commits the namespace. A killed
  node loses everything unsynced. `mmap` of a file is coherent: a write or
  truncation through a descriptor shows through its mappings at once,
  also after a failed fsync; on Linux the shared mappings of a file all
  map one memfd, so a store through one shows through the others at once,
  and on macOS each is a copy. Stores to a shared writable mapping reach
  the page cache before any call on one of the file's descriptors, and on
  `msync` (with `MS_SYNC`, an fsync), `munmap`, `mremap`, and a crash.
  Calls that wait for the device (fsync, fdatasync, `F_FULLFSYNC`,
  `O_SYNC` and `O_DSYNC` writes, `msync` with `MS_SYNC`) block the calling
  goroutine in simulated time while other goroutines and nodes run on.
  Each node's disk draws a flush latency (50µs to 20ms) and a bandwidth
  (50 MB/s to 2 GB/s); a sync waits about the latency, plus its unsynced
  bytes at the bandwidth. Faults: tail stalls of 10ms to 5s on single
  syncs, torn sector writes on a crash, fsync `EIO` with Linux's semantics
  (the dirty pages are dropped from the disk's view, stay readable until a
  power failure, and the next fsync succeeds), latent sector errors,
  `ENOSPC` (also `Node.SetDiskFull`), and opt-in bit flips and misdirected
  writes; the nemesis also slows a node's disk or stalls it until the
  fault ends. Nodes read the machine's time zones and certificates
  (`/usr/share/zoneinfo`, `/etc/ssl`, and similar paths, read-only) from
  the real OS.
- **Life cycle.** `Kill` (power failure: unsynced data lost, connections
  go silent), `KillProcess` (the kernel keeps unsynced data and closes
  the connections), `Stop(grace)` (runs `OnStop` functions, waits up to
  `grace` for the node's goroutines, then ends the process), `Pause` and
  `Resume` (the node stays on the network, so peers time out), `Throttle`
  (the node runs one period in n, so it reacts late), `Start` again
  (restart from the disk, with a new process ID: `os.Getpid` differs
  between nodes and incarnations), and `World.RemoveNode`, after which
  `World.ReAddNode` brings the node back with its disk unless
  `Node.WipeDisk` emptied it. A crash freezes the node's goroutines where
  they are, so a harness waits for the node's work on the channel
  `Node.Start` returns, which closes when the start function returns or
  the run ends, however it ends (`Node.Down()` closes only when the run
  ends), rather than on a channel its code would close. A node runs
  nothing before `Start`: `Node.Go` on it returns false.
- **Shared memory.** Nodes have their own disks, network stacks, and
  process IDs, but not their own memory: package variables are one per
  test process. A library's global logger, metrics registry, type
  registry, or pool is shared by every node and incarnation, so a node may
  see another's state, and a lock in it that a paused node holds stalls
  the others until it resumes, which looks like a liveness bug. Give each
  node its own instance where the library allows it, register global
  state once before starting nodes, or key per-process state on
  `os.Getpid()`. A frozen goroutine that holds a `sync.Mutex` or `RWMutex`
  a live node waits for runs on, as a zombie whose system calls fail with
  `EIO`, until it releases it, so a lock shared between nodes must not be
  held across a call into a node's code. A lock made of a channel (a
  `chan struct{}` of capacity 1, as code written for upstream synctest
  often uses) that a crashed node held stays held forever; under the
  experiment a goroutine waiting for a `sync.Mutex` is durably blocked,
  so use one.
- **Panics.** A panic on a node's goroutine ends the program. With
  `Config.OnPanic`, it crashes only that node, as it would on a real
  cluster, and so do runtime fatal errors raised by user code, faults on
  unmapped memory, and `os.Exit` (`log.Fatal`). The panic and its whole
  stack go to the run's log (`sim: node NAME panicked: ...`), the timeline
  says `NODE: panicked: MESSAGE`, and the summary counts each kind of
  panic, `sim.PanicKind(msg, stack)`: the message's first line, numbers
  masked, and the innermost function outside the runtime where it first
  panicked. `OnPanic` gets the node, the message, and the stack, to tell a
  crash the system meant from a bug; failing `assert.Unreachable("node panicked: "+
  sim.PanicKind(msg, stack), ...)` makes a group per kind of panic.
  Fatal errors the runtime cannot attribute to user code, such as
  `concurrent map writes`, still end the program.
- **Clock.** `SkipClock` moves every node's wall clock forward or back, on
  top of earlier skips. While a world exists, `time.Now` in the bubble
  carries a monotonic reading from the bubble's clock, so `time.Since`,
  timers, and deadlines ignore the skips. `World.Elapsed` is the
  timeline's time.
- **CPU.** A node's computing takes simulated time. Each node has a
  processor drawn from the fault stream: 1 to 16 cores, or now and then
  unlimited ones, at 0.5 to 2 times the speed of a reference core;
  `Node.SetCPU` pins it. Once one of a node's goroutines owes about a
  millisecond of the reference core in ticks (function entries and loop
  iterations of user code, 13.8ns each), it sleeps on the earliest free of
  its node's cores for as long as the work takes there, so other nodes'
  timers fire on time, and a loop that never blocks lets time pass. Each
  emulated system call costs 1µs plus 0.1ns per byte copied. The
  runtime's work, the simulation's, and goroutines of no node are free.
  `sim.BusyStats` reports the work so far.
- **No node, no network.** While a world exists, a goroutine of no node,
  such as a client the test body starts, cannot open an Internet socket:
  the call fails with `EPERM`, and the first one prints why. Run such
  code with `Node.Go` or on a node.
- **Faults.** All of the simulation's choices come from the fault stream,
  which `GODETSEED` seeds. The simulation's own faults are buggify sites:
  `sim.net.retransmit`, `sim.net.reset`, `sim.net.udpdrop`, `udpdup`,
  `udpreorder`, `ecnce`, `ecnbleach`, `ecnmangle`, `sim.disk.fsyncfail`,
  `sim.disk.slowsync`, `sim.disk.readerror`, `sim.disk.enospc`,
  `sim.disk.partialcrash` (a crash keeps some unsynced writes),
  `sim.disk.torn` (sector by sector), and the opt-in
  `sim.disk.strictdirsync` (only directory fsyncs commit entries),
  `sim.disk.bitflip`, and `sim.disk.misdirect`. They fire only once setup
  has completed (see [No faults](#no-faults)) and outside a nemesis's
  calms, and each one that fires is reported as the assertion
  `fault: SITE`, which the summary correlates with each group.
  `buggify.Disable` turns off a fault a system does not claim to survive.
- **Nemesis.** `World.NewNemesis` injects Antithesis's fault catalog, each
  fault for a random duration: partitions, clogs, and cut links in the
  three modes, restores, kills, process kills (three times as likely as
  power failures), graceful stops, pauses, throttles, clock skips, and
  disk faults (syncs 10 to 1000 times as slow, or stalled until the fault
  ends). It kills and stops nodes only with a `Restart` function. Faults
  come in storms separated by calms, which end every fault but clock
  skips and restart the nodes the nemesis took down. Each run draws its
  longest storm (5s to 2m) and calm (2s to 30s), and one in 4 is a single
  storm; each storm draws its length, its intensity (a fault every 50ms to
  5s, lasting 100ms to 20s, with at most a minority of the nodes down), a
  random subset of the kinds (swarm testing), and, one time in three, a
  node to aim at; one run in 16 has no faults. `NemesisConfig` says which
  nodes to fault (`Nodes`; `Exclude` keeps node faults off a node, such as
  a client, while network faults still reach it), how to restart one
  (`Restart`), which kinds never to inject (`Disable`), and what to call
  on each fault (`Event`). `StopFaults(d)` starts a calm of at least `d`,
  like `ANTITHESIS_STOP_FAULTS`, for checking that the system recovers,
  and `CalmSince` says whether the nemesis is in a calm, and since when.
  The nemesis reports its actions with `assert.Reachable`, in the timeline,
  and to `Event`. `Event` and `Restart` run on the nemesis's goroutine,
  which alone resumes paused nodes, so they must return promptly and must
  not wait for a node; the runtime fails the program with the callback's
  stack when one waits for a lock a paused node holds, or is blocked for a
  minute of simulated time.
- **Composer.** Package `sdk/compose` runs a workload in the shape of
  Antithesis's Test Composer. A test registers `First` (setup, with no
  faults), `Parallel`, `Serial`, and `Singleton` drivers, `Anytime`
  checks, and terminal `Eventually` (drivers killed) and `Finally`
  (drivers returned on their own) checks, which run with faults off, or
  with `FaultsOn`. A terminal check runs on a branch of the timeline: at
  random times the composer forks the process (`sim.Branch`), the branch
  runs the check and exits, and the timeline goes on, so one timeline
  checks recovery from many of its states. A branch reports only the
  assertions that fail in it; after the timeline has failed, branches
  still run and show its symptoms, under the timeline's failure in the
  summary. The composer runs the nemesis during the drive phase, restarts
  the drive phase after a fork of the search, and reports which commands
  overlap which faults. Commands run on a client node that the nemesis
  never kills; with `Config.Clients` above 1, on one of several, which can
  be on different sides of a partition. `testdata/simcompose` is an
  example.

The search's forks work with simulated sockets and files, since the
simulation's state is all in memory. `testdata/simkv` exercises the life
cycle and `testdata/simfaults` each fault.

## How the runtime makes runs deterministic

All randomness (map seeds and order, `select` order, hash keys, the heap
base address, the `math/rand` globals, `-test.shuffle=on`, and all
cryptographic randomness) comes from `GODETSEED`, a decimal uint64.
A map key that holds a pointer (a pointer, channel or `unsafe.Pointer`,
alone or in a struct, array or interface, in a map, a `sync.Map`,
`unique.Make` or `maphash.Comparable`) hashes by the pointer's
identity: a number given out in the order in which the program first
stores each pointer in a map, which the pointer keeps until its object is
freed. So map order does not depend on where objects are in memory,
which differs between an optimized build and one without optimizations,
and with anything that moves an allocation. A lookup of a pointer that
no map holds gives it no identity. A map operation on such a key costs
about 10ns more, and storing a pointer for the first time 100 to 200ns
more. With
it set, the runtime also perturbs the schedule, prints the trace line at
exit, and reports where the program leaves the deterministic subset.
Without it, a program built with the experiment runs one unperturbed
schedule of seed 0 and prints nothing extra.

Goroutines run on one P and are preempted after a fixed amount of work:
the compiler counts a tick at the entry of every function that calls
something or loops, inlined or not, and on every loop iteration, outside
the runtime, where the source has them, so a build without optimizations
counts the same ticks. It emits a synchronization point before every
channel operation, `select`, `go` statement, and atomic operation, where
the perturbation can switch goroutines too. A goroutine in a system call
keeps the only P; after 10 ms, a watchdog thread hands it off so other
goroutines can run. cgo callbacks are not supported, and `go build -race`
fails, because the race detector randomizes the scheduler.

Writes to standard output and error through package `syscall` go to an
in-memory buffer, written at exit, so no goroutine waits on the OS; the
runtime's own messages, such as panics, are written directly. A child
process built with the experiment buffers its output too, so a parent
can't talk to it over pipes. `os.Stdin`, `os.Stdout` and `os.Stderr` never
use the network poller; a read from a nonblocking standard input with
nothing to read fails with `EAGAIN`.

Garbage collection never changes the schedule. Finalizers, cleanups, weak
pointers, and `sync.Pool` emptying wait for an epoch: a call of
`runtime.GC`; the first tick after the program set 4096 finalizers,
cleanups, and weak pointers; or a point where no goroutine can run,
before the program idles or reports a deadlock. So a goroutine that
blocks until a finalizer or cleanup runs sees it run, but one that waits
for one by sleeping in a loop inside a bubble keeps the bubble's clock
jumping instead; call `runtime.GC` there. Finalizers and cleanups run
outside every bubble, as upstream, so they must not use a bubble's
channels or stop its timers.

synctest bubbles never wait in real time. When no goroutine anywhere can
run, and none is in a system call or waiting on the network, the blocked
goroutines in a bubble count as durably blocked, even those blocked on a
mutex or a channel created outside the bubble, which upstream would leave
waiting: a pending `synctest.Wait` returns, or the bubble's clock jumps to
its next timer, or `synctest.Test` reports a deadlock. Timers outside
bubbles still use real time.

A forked run is identical to re-executing its tape. Subprocesses started
before a fork are not the children's children, so a program that forks
keeps to simulated files and starts no processes. Runs of one seed name
their temporary files alike (`os.MkdirTemp`, `os.CreateTemp`,
`t.TempDir`); a run that finds its name taken by another run of its seed
takes a random one of the same length, which leaves its schedule
unchanged.

## macOS

On macOS on arm64 the experiment works as on Linux, with these
differences.

- **ASLR.** macOS loads every program at a random address, which would
  change the addresses the program sees and so the schedule. A program built with the
  experiment that finds itself moved runs itself again in its place with
  ASLR off (`posix_spawn` with `_POSIX_SPAWN_DISABLE_ASLR`) before it does
  anything else, keeping its process ID. Under a debugger, start it
  without ASLR (`dlv exec --disable-aslr`), or it loses every breakpoint
  and says so (`detsched: running again without ASLR, so a debugger loses
  its breakpoints`).
- **Fork mode.** The child gets new threads for the runtime's other Ms and
  a new kqueue. A fork server starts with
  `OBJC_DISABLE_INITIALIZE_FORK_SAFETY=YES`, so that a child can verify
  certificates; a process that has used the system's verifier cannot
  fork, because its connection to `trustd` does not survive fork.
- **Simulation.** System calls go through libc, so the simulation maps
  each libc function the runtime calls to its own handler, with darwin's
  flags, errno values, `stat` and `sockaddr` layouts, and `fcntl`
  (`F_FULLFSYNC` is an fsync). A direct system call through
  `syscall.Syscall` with a trap number is not simulated.
- A breakpoint hit costs about 13ms, against 1ms on Linux.
- darwin on amd64, and iOS, are not supported.

## Corpus

`testdata` holds the programs that the tests in `cmd/detsched` run; each
package comment says what it exercises and which test runs it.

- Determinism: `hello`, `maporder`, `selectorder`, `cryptorand`, `shuffle`,
  `gcheap`, `gcstress`, `mutexstarve`, `synctest`, `nonidle`, `spin`,
  `godebugkeys` and `output` run the same way for a seed, and the seed
  picks what they see of the runtime's randomness. `pid` talks to the OS
  and is the control that must differ.
- Compiler and runtime regressions: `preemptentry` and `rangegc` (run with
  a quantum of a few ticks), `spinwait`, `assertall`.
- Bugs the search must find: `bugorder`, `bugcache`, `buglockorder`,
  `bugatomic`, `bugrarelock`, `sdk`, `setupbugs` and `stagedbug`, and the
  fork cases `forkidle` and `forkpoll`. Which seed finds a bug first
  depends on the binary, so on the architecture.
- The simulation: the `sim*` packages, such as `simfaults` for each fault
  and `simcompose` for `sdk/compose`.
- `delve`: the bundled Delve's tests on a failing replay of `simcompose`.

`go test` in `cmd/detsched` runs the corpus and checks the command itself,
in a few minutes on four cores.
