# detsched

`GOEXPERIMENT=detsched` makes Go programs that run their concurrency inside
`testing/synctest` bubbles deterministic: one seed gives one schedule, every
time. Bubbles never wait in real time, so a test that sleeps for an hour of
virtual time takes as long as its computation. The `detsched` command uses
that to search a package's tests for schedules (and, with
[`sdk/sim`](#simulating-a-cluster), faults) in which they fail, and prints a
command that replays each failure exactly, under a debugger too.

This is a fork of Go 1.27.4 for testing only: all randomness, including
`crypto/rand`, comes from the seed. It runs on Linux (amd64, arm64) and
macOS on arm64.

## Getting started

Download a release for your platform from
https://github.com/ryanworl/go-detsched/releases and put its `bin` on
your `PATH`; it has `go` and `detsched`:

```
v=go1.27.4-detsched17
mkdir -p ~/sdk
curl -fsSL https://github.com/ryanworl/go-detsched/releases/download/$v/$v.linux-amd64.tar.gz | tar -C ~/sdk -xz
export PATH=$HOME/sdk/$v/bin:$PATH
```

Use `linux-arm64` or `darwin-arm64` for those platforms. The macOS
binaries are not notarized: download them with curl, or run
`xattr -d com.apple.quarantine` on an archive a browser downloaded before
unpacking it. Or build the toolchain and the command from this tree,
which puts `detsched` next to `go`:

```
misc/release/build.bash
export PATH=$PWD/bin:$PATH
```

Then run it on a package whose tests use `testing/synctest`:

```
detsched ./pkg
```

That is all there is to it. detsched builds the package's test binary with
the experiment, runs it on every core with one seed after another until you
press Ctrl-C or send it SIGTERM (or `-duration` passes, or `-n` runs have
run), and shows progress as it goes: on a terminal, a status line updated in
place; otherwise, a line every 30 seconds. It also writes the summary so far
to a file in its directory every 30 seconds, and says where when it starts.
Its counts are runs, failed runs,
signatures (groups of failures), and features: the new states the guided
search reached (see [the search](#the-search)), which stop growing once it
has found most of what it can reach. For each distinct failure it prints the log of its
first run, a command that replays it, and the same command under Delve,
and checks that the replay reproduces the failure:

```
$ detsched ./testdata/bugcache
FAIL panic: computed answer twice [recovered, repanicked]
	log:    /tmp/detsched-334332647/fail-1-1.log
	replay: cd /home/me/go/misc/detsched/testdata/bugcache && GODETSEED=1 GODETBRANCH=0 GODETSEARCH=trace=0x5a28688c3de0741d /tmp/detsched-334332647/bugcache.test
	debug:  cd /home/me/go/misc/detsched/testdata/bugcache && GODETSEED=1 GODETBRANCH=0 GODETSEARCH=trace=0x5a28688c3de0741d dlv exec --disable-aslr /tmp/detsched-334332647/bugcache.test
^C
detsched: ./testdata/bugcache: 489 runs, 174 failed, 1 signatures, 0 features, 736.0 runs/s, 1s
...
FAIL 174 of 489 runs: panic: computed answer twice [recovered, repanicked]
	log:    /tmp/detsched-334332647/fail-1-1.log (and 9 more of these runs: /tmp/detsched-334332647/fail-1-*.log)
	replay: cd /home/me/go/misc/detsched/testdata/bugcache && GODETSEED=1 GODETBRANCH=0 GODETSEARCH=trace=0x5a28688c3de0741d /tmp/detsched-334332647/bugcache.test
	debug:  cd /home/me/go/misc/detsched/testdata/bugcache && GODETSEED=1 GODETBRANCH=0 GODETSEARCH=trace=0x5a28688c3de0741d dlv exec --disable-aslr /tmp/detsched-334332647/bugcache.test
```

The `debug:` line runs Delve's `dlv`, which the release archive has in
`bin/`, next to `go` and `detsched`.

A search keeps its binary, logs, and summary in a new directory under
`$TMPDIR` (else `/tmp`), which it prints at its start; set `TMPDIR` to
keep them elsewhere. A search that finds no failure removes all but the
summary.

Flags, all optional:

- `-run regexp` runs only the matching tests, as `go test -run` does.
- `-duration d` stops after `d`; `-n N` stops after N runs, so that a CI job
  checks the same N runs every time. Either then runs seed 1 once more, to
  check that its schedule has not changed, before the summary; SIGINT or
  SIGTERM skips that run.
- `-timeout d` (default 10m) stops a run that takes longer, with `SIGQUIT`,
  so that its log shows every goroutine, and counts it as a failure. The
  summary prints the slowest passing and failing runs to choose it from,
  with the slowest passing run's replay line when it took 5 seconds or more,
  and the progress line counts the runs going on for over 10 times as long
  as the slowest passing run, which may hang, before `-timeout` stops them.
  When seed 1 passes, a forked run that makes 1000 times its goroutine
  switches is taken to spin and stopped sooner; the summary counts such runs
  apart, not as failures, with the replay line of one.
- `-p n` (default: one fewer than the number of CPUs) runs `n` programs at
  once. Lower it when each run takes enough memory that a run per CPU would
  make the machine swap. detsched and its runs run at nice 10, so a search
  yields the CPUs to the rest of the machine's work, and the CPU it leaves
  free keeps a virtual machine whose host limits its CPU time as a whole
  responsive.
- `-seed-base n` adds `n` to every seed after seed 1, so that machines
  splitting a search run disjoint seeds: with `-seed-base $((i << 32))`,
  shard `i` runs seeds `i<<32+2`, `i<<32+3`, and so on. Replay lines print
  the seed itself.
- `-exec binary` runs a prebuilt binary in the current directory instead of
  building a package, so that one job builds it and every shard runs the
  same file. detsched runs a copy it keeps with the logs, which replay
  lines name, so rebuilding the binary changes neither a search nor its
  replay lines. Build it as detsched does, from
  the package's directory:

  ```
  GOEXPERIMENT=detsched CGO_ENABLED=0 go test -c -o pkg.test
  detsched -exec ./pkg.test
  ```

  It also compares two versions of the code: build a binary from each and
  search both with the same `-n`, then compare their failure rates.

detsched exits with status 1 if any run failed.

### What a run is

A run executes the test binary once, in the package's directory, with the
same arguments every time. Failing runs are grouped by their first assertion
failure ([`sdk/assert`](#the-sdk)), panic, or fatal error, else their first
failing test, with numbers masked and an assertion's details left out;
detsched keeps the logs of the first ten runs of each group, group G's as
`fail-G-1.log` to `fail-G-10.log`. A log starts with the run's replay line
and its failures, each after `# `. The summary's heading of a group gives
the message without details, and the details of its first run on the next
line. Under each group, the summary lists the other failures its runs had
(`also in N of these runs: ... (first in fail-G-K.log)`), such as the
symptoms that followed the group's failure, and detsched keeps the log of
the first run with each, whose first line replays it. When runs failed more
than one way, the summary also counts the runs that failed each way, first
or after another failure. A run keeps at most 16 MB of
its output, its first and last 8 MB, with a line where the rest was cut. A
failure inside a branch of a [composer](#simulating-a-cluster) timeline is
reported as that branch's failure: its log starts with the branch's output,
which ends with the branch's fault timeline (`sim: fault timeline of branch
K:`), and goes on with the timeline's (`sim: fault timeline of the timeline
(branch K's is above):`).

The summary also lists the program's `assert.Sometimes` and
`assert.Reachable` assertions with the runs each held in, those never true
first, so that a search that passes still shows which paths it reached,
and on one line each, the runs that reached each fault of the nemesis,
each feature of the composer, and each kind of node panic that
`sim.Config.OnPanic` caught. An assertion counts as reached in a run if
the run or any branch it forked reached it.
An assertion that no run reached is not listed.

The first run, with seed 1, runs twice, and once more when the search
ends, minutes later in real time. If its runs differ, the program
depends on something besides its seed, such as real time or the OS, and
detsched warns that failures may not replay. The runtime reports, with the
goroutine's stack, each place where a program leaves the deterministic
subset; see [the runtime](#how-the-runtime-makes-runs-deterministic).

### The search

Each seed runs another schedule: the runtime perturbs the order in which
goroutines run, choosing for each seed between a random walk and PCT
(probabilistic concurrency testing) and their parameters. If the program
reports the end of its setup (`lifecycle.SetupComplete`), detsched runs
setup once in each of a few seeds and forks the stopped processes, each
fork reseeded right after setup, so fresh seeds don't pay for setup. On top
of fresh seeds:

- A run that reaches a state no run reached before makes it a cell. Since
  runs are deterministic, a cell is just the run's seed and branch points
  and the switch where it got there. To explore a cell, detsched re-runs it
  to that switch, keeps the process stopped there while it uses the cell,
  and forks runs from it, each with new random choices after the fork, so
  a failure that needs several rare events in a row is found one event at
  a time. A state is new when the program reports a new `guide.State` or
  assertion outcome, if it reports guide states at all, else when the
  runtime records a new place where goroutines park, a new `select` case,
  a new lock order, a recovered panic, or a new maximum of goroutines,
  channel buffers, or virtual time. A run makes at most one cell, at the
  first new state it reached.
- When a run takes two mutexes in the order opposite to another goroutine's,
  the lock order predicts a deadlock, and some runs then make goroutines
  that take the first of those mutexes yield while they hold it, so that
  the other goroutine can take the second. The summary lists the predicted
  deadlocks, with how many runs were directed at them and how many of
  those failed. A prediction can be a false alarm: mutexes are told apart
  by address, and the memory of one freed may be reused for another, as
  happens with `context` cancellations.

detsched spends its runs on fresh seeds, cells, and directed runs in
proportion to the new cells and failures each produced lately per second,
and gives each at least a tenth of the runs; it picks cells the same way.
A program that cannot fork, such as one with real network connections
open, runs fresh seeds only.
While less than a quarter of the memory is available, detsched closes its
stopped processes and starts no runs; below a tenth, it stops the run that
has run longest and does not count it as a failure.

### Replaying a failure

Run the printed command as it is. It runs the kept binary in the package's
directory with the run's seed (`GODETSEED`), the number of the composer
branch it failed in, or 0 for none (`GODETBRANCH`), and, for a run forked
from another or directed at a deadlock, `GODETSEARCH`, which holds the
points where it forked and the sites it yielded at, and the run's schedule
hash (`trace=`): a replay whose schedule differs says so at exit
(`detsched: warning: this run's schedule is not the one its replay line was
printed for`). It also sets the variables of detsched's environment that
change seed 1's schedule, such as one a test reads to plant a bug, or
`NO_COLOR`, which logging libraries read at init, which detsched finds by
running seed 1 without groups of its variables, and every other variable
the program read (the runtime records their names), unsetting with
`env -u` those it read that detsched's environment did not have, so the
command runs the same in a new shell whatever that shell sets. Change
nothing: another binary, other arguments
(even `-test.v` or `-test.run`), or a line of debugging output added to the
program changes the schedule. Code that prints diagnostics should run only
after an assertion has failed (`assert.Failures`), and never behind an
environment variable that a passing run reads.

To debug the failure, run the `debug:` line, the same command under
Delve: `--disable-aslr` keeps dlv's breakpoints on macOS, where the
program restarts itself without ASLR, and does nothing on Linux.
[delve.md](delve.md) shows how to trace and inspect a replay without
changing it.

`GODETBRANCH` makes a composer timeline run its failing branch in the
process itself rather than in a forked child, which a debugger does not
follow, and with 0 runs the timeline without forking at all. To find where
two runs diverge, run both with `GODEBUG=dettrace=2`, which prints every
goroutine switch, and diff their standard error.

`GODETSTOP` stops a replay at points in virtual time (`t9.4`), at a
switch (`s12345`), or at a tick (`k812345678`, one per function entry and
loop back edge), so that a debugger's breakpoints can be armed only
between two points instead of trapping on every hit before them, and
going back is replaying to an earlier tick; `:dump` prints every
goroutine's stack there instead. It does not change the run.
[delve.md](delve.md#stop-at-a-point-of-the-run) shows how.

## The SDK

Packages under `sdk` let a test tell detsched what matters. They are the
module `github.com/ryanworl/go-detsched/misc/detsched` in this directory,
and release N's version is v0.N.0, so with go1.27.4-detsched17:

```
go get github.com/ryanworl/go-detsched/misc/detsched@v0.17.0
```

Import them as `github.com/ryanworl/go-detsched/misc/detsched/sdk/assert`
and so on. Without the experiment they do nothing, or print, so tests
build and pass with stock Go.

- `assert`, after the Antithesis SDK: `Always`, `AlwaysOrUnreachable`,
  `Sometimes`, `Reachable`, and `Unreachable`, identified by their message.
  Under the experiment, each failing assertion is reported once at exit,
  with its first failure's details and how many more times it failed, as
  `detsched: assertion failed: MESSAGE DETAILS (and N more failures)`; a
  breakpoint on `runtime.detAssertFailed` stops at every failure. A test
  during which an
  assertion failed fails too, so that its log is printed, and a run that
  would exit with status 0 exits with status 3. `assert.Failures` counts
  the failures so far, so a test can print its history after one.
- `buggify`, after FoundationDB's BUGGIFY: `buggify.On("site")` marks a
  place to inject a fault. Each run enables a quarter of the sites, and an
  enabled site fires a quarter of the time. `Disable` turns sites off.
- `guide`: `guide.State(name, s)` reports an abstract state of the program,
  such as each node's role, and `Maximize` a value to push up. When a
  program reports states, only they and assertions make a run new.
- `lifecycle`: `SetupComplete` marks the end of an expensive setup, which
  the search then runs once per seed rather than once per run, and
  `Branches` tells a test with a fixed-length workload to count it from the
  last fork.
- `sim` and `compose` simulate a cluster; see below.

## How the runtime makes runs deterministic

All randomness (map seeds and order, `select` order, hash keys, the heap
base address, the `math/rand` globals, `-test.shuffle=on`, and all
cryptographic randomness, including `crypto/rand`) comes from `GODETSEED`, a
decimal uint64. With `GODETSEED` set, as detsched sets it, the runtime also
perturbs the schedule, prints a trace line at exit
(`detsched: trace=... switches=... syncs=...`), and reports where the program
leaves the deterministic subset. Without it, a program built with the
experiment runs one unperturbed schedule of seed 0 and prints nothing extra.

Goroutines run on one P and are preempted after a fixed amount of work: the
compiler counts a tick at every function entry and loop backedge outside the
runtime, and emits a synchronization point before every channel operation,
`select`, `go` statement, and atomic operation, where the perturbation can
switch goroutines too. A goroutine in a system call keeps the only P; if the
call lasts 10 ms, a watchdog thread hands the P off so other goroutines can
run, as sysmon does upstream. cgo callbacks are not supported, and
`go build -race` fails, because the race detector randomizes the scheduler.

Writes to standard output and error through package `syscall` go to an
in-memory buffer, written at exit, so no goroutine waits on the OS to accept
output. The runtime's own messages, such as panics, are written directly. A
child process built with the experiment also buffers its output until it
exits, so a parent can't talk to it interactively over pipes. `os.Stdin`,
`os.Stdout` and `os.Stderr` never use the network poller, even when they
are nonblocking, so that how the program was started doesn't change the
schedule; a read from a nonblocking standard input with nothing to read
fails with `EAGAIN`.

synctest bubbles never wait in real time. When no goroutine anywhere can
run, and none is in a system call or waiting on the network, the blocked
goroutines in a bubble count as durably blocked, even those blocked on a
mutex or a channel created outside the bubble, which upstream would leave
waiting: a pending `synctest.Wait` returns, or the bubble's clock jumps to
its next timer, or `synctest.Test` reports a deadlock. Timers outside
bubbles still use real time.

The runtime reports, once per distinct stack, each place where a program
leaves the deterministic subset: waiting for a file or network connection to
become ready, staying in a system call long enough for the watchdog to hand
off the P, starting a subprocess, a timer outside every bubble firing, and
the bubble rule above acting where upstream would hang. Each report says
where in the run it happened, as `at sN kN`, the switch and tick that
`GODETSTOP` takes, and with `GODEBUG=dettrace=2` it is made every time,
among the switches. detsched prints the reports of the first run, seed 1,
when the search starts. The runtime does
not catch system calls that return data from the OS, such as the process ID
or file contents.

A forked run is identical to re-executing its tape. A program refuses to
fork while `runtime.iscgo` is set, which detsched's `CGO_ENABLED=0` build
leaves unset unless a package sets it, as
`github.com/ebitengine/purego/internal/fakecgo` does; during package
initialization; while a goroutine is in a system call or locked to its
thread; while a file is registered with the network poller; and on macOS
once it has used the system's certificate verifier. detsched then runs
without forking. Real files open across a fork
share their offsets, locks, and shared mappings between processes, and
subprocesses started before it are not the children's children, so a
program that forks should keep to simulated files and start no processes.

## Simulating a cluster

Package `detsched/sdk/sim` runs the nodes of a distributed system in one
process, each with its own simulated network stack and disk, so that
unmodified code using `net` (TCP and UDP over IPv4) and `os` talks over a
simulated network and stores data on a simulated disk. Create a `World`
inside a synctest bubble, add nodes with `AddNode(name, ip)`, and start
each with `Start(f)`: `f` runs in a goroutine that belongs to the node,
and so do the goroutines it starts, also through `time.AfterFunc`. On
those goroutines, path and socket system calls go to the simulation,
which also owns the descriptors it returns, on any goroutine. Other
goroutines use the real OS. Linux on amd64 and arm64, and macOS on arm64
(see [macOS](#macos)).

A harness needs little more than that, and a `Nemesis` (or the composer,
below) to inject faults:

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

A run that fails, by an assertion, a panic, or a failed test, prints the
fault timeline after its output: every start, crash, stop, pause, disk
fault, clock skip, and nemesis fault, and the first failure of each
assertion while the world exists, under the node whose goroutine failed it
(`world` for one of no node), with the simulated time, such as
`sim: 12.004000000 n1: power failure`. A run that passes prints none of
it, so the replay command a failure prints shows the timeline again.

- **Network.** Every write becomes segments that arrive after a latency
  drawn from the fault stream, in order on their connection. Links have a
  latency of 1ms plus an exponential jitter with a mean of 1ms; `SetLink`
  changes a directed link (latency, jitter, MTU, and a drop rate, where a
  lost packet shows up as a TCP retransmission delay). Half of the worlds
  round arrivals up to whole milliseconds, so that messages and timers
  coincide and the scheduler decides their order. Network faults act on
  every segment, including on established connections, in one of three
  modes: `Stopped` (held until the fault ends, or the connection fails
  with `ETIMEDOUT` after 30s), `Slowed` (added latency with a mean and
  deviation), or `Jammed` (held, then delivered, never timing out).
  `Partition`, `Isolate`, and `Block` stop traffic between groups, around
  a node, or on one link; `PartitionGroups`, `Clog`, and `Cut` do the same
  in any mode. Each returns a `NetFault` to `Heal`, and `World.Heal` ends
  them all. A connection attempt that cannot get through retries for 20s.
  TCP has Linux's windows (4 MiB in flight, 6 MiB unread) and keepalive.
  Deadlines and timeouts use the bubble's clock.
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
  node loses everything unsynced. `mmap` of a file is a coherent copy: a
  write or truncation through a descriptor shows through its mappings at
  once, as the page cache does, also after a failed fsync. On
  Linux, the shared mappings of a file all map one memfd, so that a store
  through one shows through the others at once; on macOS each is a copy
  too. Stores to a shared writable mapping reach the file's page cache
  before any call on one of the file's descriptors, and on `msync` (with
  `MS_SYNC`, an fsync), `munmap`, `mremap`, and a crash of the process.
  Calls that wait for the device (fsync, fdatasync, `F_FULLFSYNC`,
  `O_SYNC` and `O_DSYNC` writes, and `msync` with `MS_SYNC`) block the
  calling goroutine in simulated time while the node's other goroutines
  and the other nodes run on. Each node's disk draws a flush latency
  (50µs to 20ms) and a bandwidth (50 MB/s to 2 GB/s), so runs cover fast
  and slow disks; a sync waits about the latency, plus its unsynced bytes
  at the bandwidth. Faults: tail stalls of 10ms to 5s on single syncs,
  torn sector writes on a crash, fsync `EIO` with Linux's semantics (the
  dirty pages are dropped and the next fsync succeeds), latent sector
  errors, `ENOSPC` (also `Node.SetDiskFull`), and opt-in bit flips and
  misdirected writes; the nemesis also slows a node's disk or stalls it
  until the fault ends.
- **Life cycle.** `Kill` (power failure: unsynced data lost, connections
  go silent), `KillProcess` (the kernel keeps unsynced data and closes the
  connections), `Stop` (runs `OnStop` functions, waits for the node's
  goroutines, then ends the process), `Pause` and `Resume` (the node stays
  on the network, so peers time out), `Throttle` (the node runs one period
  in n, so it reacts late), `Start` again (restart from the disk, with a
  new process ID: `os.Getpid` differs between nodes and between the
  incarnations of one), and `World.RemoveNode`, after which `World.ReAddNode`
  brings the node back with its disk unless `Node.WipeDisk` emptied it. A
  crash freezes the node's goroutines where they are. Nodes share the
  process's memory, such as libraries' global state: a frozen goroutine
  that holds a `sync.Mutex` or `RWMutex` a live node waits for runs on, as
  a zombie whose system calls fail with `EIO`, until it releases it. So a
  lock shared between nodes must not be held across a call into a node's
  code, where a zombie may wait for its node's frozen goroutines. Only
  those two types are released this way: a lock made of a channel (a
  `chan struct{}` of capacity 1, as code written for upstream synctest
  often uses, since upstream never counts a goroutine waiting for a
  `sync.Mutex` as durably blocked) that a crashed node held stays held,
  and every live node that takes it waits forever. Under the experiment a
  goroutine waiting for a `sync.Mutex` is durably blocked, so use one.
- **Shared state.** The simulation gives each node its own disk,
  network stack, and process ID, but not its own memory: package
  variables are one per test process, not per node. A library's global
  logger, metrics registry (Prometheus's default registry), type registry
  (protobuf's), or pool (zstd's) is shared by every node and every
  incarnation, so a node may see another's state, and a lock in it that a
  paused node holds stalls the others until it resumes, which looks like
  a liveness bug. Give each node its own instance where the library
  allows it, register global state once before starting nodes, or key
  per-process state on `os.Getpid()`.
- **Panics.** A panic on a node's goroutine ends the program, as a bug.
  With `Config.OnPanic`, it crashes only that node, as it would crash only
  that process on a real cluster, and so do runtime fatal errors raised by
  user code, faults on unmapped memory, and `os.Exit` (`log.Fatal`); `OnPanic` gets the node, the
  message, and the stack, to tell a crash the system meant from a bug.
- **Clock.** `SkipClock` moves every node's wall clock forward or back, on
  top of earlier skips. While a world exists, `time.Now` in the bubble
  carries a monotonic reading from the bubble's clock, so `time.Since`,
  timers, and deadlines ignore the skips.
- **CPU.** A node's computing takes simulated time (busy time). Each node
  has a processor drawn from the fault stream: 1 to 16 cores, or now and
  then unlimited ones, at 0.5 to 2 times the speed of a reference core;
  `Node.SetCPU` pins it. The runtime counts a node's work in ticks
  (function entries and loop backedges of user code), and once one of its
  goroutines owes about a millisecond of the reference core, it sleeps on
  the earliest free of its node's cores for as long as the work takes
  there. Other nodes run on meanwhile, so their timers fire on time, and a
  loop that never blocks, such as a retry without a backoff, lets time
  pass. Each emulated system call costs 1µs plus 0.1ns per byte copied.
  The runtime's own work, the simulation's, and the work of goroutines of
  no node are free. A tick stands for 13.8ns of the reference core,
  calibrated against one core of a 2.6GHz Xeon on Pebble and
  hashicorp/raft workloads (the calibration harness is not published).
  `sim.BusyStats` reports the work so far.
- **Real files.** Nodes read the machine's time zones and certificates
  (`/usr/share/zoneinfo`, `/etc/ssl`, and similar paths, read-only) from
  the real OS; everything else is on the node's disk.
- **No node, no network.** While a world exists, a goroutine of no node,
  such as a client the test body starts, cannot open an Internet socket:
  its dial or listen would wait on the real network, which stalls the
  deterministic scheduler. The call fails with `EPERM`, and the first one
  prints a message saying to run the code with `Node.Go` or on a node.
- **Names.** Nodes have no resolver: they dial IP addresses.
- **Faults.** All of the simulation's choices come from a third random
  stream, the fault stream, which `GODETSEED` seeds. Package
  `detsched/sdk/buggify` marks fault sites, after FoundationDB's BUGGIFY:
  each run enables a quarter of the sites (swarm testing), and an enabled
  site fires a quarter of the time. The simulation's own faults are sites
  too: `sim.net.retransmit`, `sim.net.reset`, `sim.net.udpdrop`,
  `udpdup`, `udpreorder`, `ecnce`, `ecnbleach`, `ecnmangle`,
  `sim.disk.fsyncfail`, `sim.disk.slowsync` (a sync stalls 10ms to 5s),
  `sim.disk.readerror`, `sim.disk.enospc`,
  `sim.disk.partialcrash` (a crash keeps some unsynced writes),
  `sim.disk.torn` (sector by sector), and the opt-in
  `sim.disk.strictdirsync` (only directory fsyncs commit entries),
  `sim.disk.bitflip`, and `sim.disk.misdirect`. `buggify.Disable` turns
  sites off for a system that does not claim to survive them, and
  `buggify.Enable` allows opt-in sites.
- **Nemesis.** `World.NewNemesis` injects Antithesis's fault catalog, each
  fault for a random duration: partitions, clogs, and cut links in the
  three modes, restores, kills, process kills (three times as likely as
  power failures), graceful stops, pauses, throttles, clock skips, and
  disk faults (a node's syncs take 10 to 1000 times as long, or stall
  until the fault ends). It
  kills and stops nodes only with a `Restart` function. Faults come in
  storms separated by calms, which end every fault but clock skips, which
  stay as an NTP step would, and restart the nodes the nemesis took down. Each run draws its longest storm (5s to 2m) and
  calm (2s to 30s), and one in 4 is a single storm. Each storm draws its
  length (from 2s), its intensity (a fault every 50ms to 5s, lasting 100ms
  to 20s, as many overlapping as that makes, with at most a minority of
  the nodes down), a random subset of the kinds and modes (swarm testing),
  and, one time in three, a node to aim at; each calm draws its length
  (from 1s), and one run in 16 has no faults. `NemesisConfig` says which
  nodes to fault (`Nodes`; `Exclude` keeps node faults off a node, such
  as a client or a coordinator, while network faults still reach it),
  how to restart one (`Restart`),
  which kinds never to inject (`Disable`; `FaultClockSkipBack` keeps the
  clock from stepping back), and what to call on each fault (`Event`).
  `StopFaults(d)` starts a calm of at least `d`, like
  `ANTITHESIS_STOP_FAULTS`, for checking that the system recovers. The
  nemesis reports its actions with `assert.Reachable`, in the fault
  timeline, and to `Event`, through which a harness tracks faults against
  its own state, such as whether the node killed was the leader.
  `CalmSince` reports whether the nemesis is in a calm, and since when,
  for checks that need a healthy cluster; it is safe on any goroutine and
  changes only once faults have ended or before they start, which a
  harness tracking `Event` itself tends to get wrong. `Event`
  and `Restart` run on the nemesis's goroutine, which alone resumes the
  nodes it paused, so they must return promptly and must not take locks
  that node goroutines hold, nor wait for a node in any other way. The
  runtime fails the program instead, with the callback's stack: at once
  when a callback is about to wait for a `sync.Mutex` or `RWMutex` that a
  goroutine of a node the nemesis paused holds, naming the node, and once
  a callback has been blocked for a minute of simulated time.
- **Composer.** Package `detsched/sdk/compose` runs a workload in the shape
  of Antithesis's Test Composer. A test registers `First` (setup, with no
  faults), `Parallel`, `Serial`, and `Singleton` drivers, `Anytime`
  checks, and terminal `Eventually` (drivers killed) and `Finally` (drivers
  returned on their own) checks, which run with faults off, or with
  `FaultsOn`. As in Antithesis, a terminal check runs on a branch of the
  timeline: at random times the composer forks the process
  (`sim.Branch`), the branch runs the check and exits, and the timeline
  goes on, so one timeline checks recovery from many of its states. A
  branch reports only the assertions that fail in it. After the timeline
  has failed, branches still run, so their checks show the failure's
  symptoms, such as lost writes: their output comes after the timeline's
  report, and the summary lists them under the timeline's failure. A
  debugger does not follow the fork: `GODETBRANCH=N`, with the same
  binary and environment as the failing run, runs the Nth branch in the
  process itself, with the same schedule as the forked branch, and the
  composer's output for a failing branch names its `GODETBRANCH`. The
  composer picks from the fault stream which command
  runs when and alongside what, runs the nemesis during the drive phase,
  restarts the drive phase after a branch of the search, and reports
  which commands overlap which faults to the search. Commands run on a
  client node that the nemesis never kills, though partitions put it on
  either side, with some servers and not others. With `Config.Clients`
  above 1, each command runs on one of that many client nodes, so two
  clients can be on different sides of a partition, as a stale read from
  a deposed leader needs. `testdata/simcompose` is an example.

The search's forks work with simulated sockets, which they refuse for
real ones: the simulation's state is all in memory. `testdata/simkv` exercises the
life cycle and `testdata/simfaults` each fault. The harnesses for real
systems that detsched was developed against, such as an unmodified
hashicorp/raft cluster, are not published.

## Corpus

`testdata` holds the programs that the tests in `cmd/detsched` run; each
package comment says what it exercises and which test runs it.

- Determinism: `hello`, `maporder`, `selectorder`, `cryptorand`, `shuffle`,
  `gcheap`, `gcstress`, `mutexstarve`, `synctest`, `nonidle`, `spin`,
  `godebugkeys` and `output` run the same way for a seed, and the seed picks
  what they see of the runtime's randomness. `pid` talks to the OS and is
  the control that must differ.
- Compiler and runtime regressions: `preemptentry` and `rangegc` (run with a
  quantum of a few ticks), `spinwait`, `assertall`.
- Bugs the search must find: `bugorder`, `bugcache`, `buglockorder`,
  `bugatomic`, `bugrarelock`, `sdk`, `setupbugs` and `stagedbug`, and the
  fork cases `forkidle` and `forkpoll`. Which seed finds a bug first depends
  on the binary, so on the architecture.
- The simulation: the `sim*` packages, such as `simfaults` for each fault
  and `simcompose` for `sdk/compose`.

`go test` in `cmd/detsched` runs the corpus and checks the command itself,
in about a minute on four cores.

## macOS

On macOS on arm64 the experiment works as on Linux, with these
differences.

- **ASLR.** macOS loads every program at a random address, which would
  change the addresses that hash and order maps, and so the schedule. A
  det-mode program that finds itself moved runs itself again in its place
  with ASLR off (`posix_spawn` with `_POSIX_SPAWN_DISABLE_ASLR`), before it
  does anything else, so its process ID is that of the first run. A
  set-user-ID or set-group-ID program doesn't, because the second run
  would lose secure mode, so its schedule may depend on where it is loaded.
- **Fork mode.** macOS supports `fork` without `exec` only in part. The
  child gets new threads for the runtime's other Ms, which first
  reinitialize their pthread mutexes and condition variables, and a new
  kqueue, which fork does not inherit. A fork server starts with
  `OBJC_DISABLE_INITIALIZE_FORK_SAFETY=YES`, which stays in its
  environment, so that a child can verify certificates with the system's
  verifier (crypto/x509 reaches Objective-C through Security.framework).
  The program sees the variable only if it was set already, so it sees
  the same environment as when a debugger starts it without ASLR.
  A process that has used the verifier cannot fork, because
  Security.framework's connection to `trustd` does not survive fork; it
  refuses to, and detsched runs without forking. macOS has no
  `SOCK_SEQPACKET` Unix sockets, so detsched's control socket is a stream.
- **Simulation.** System calls go through libc, so the simulation maps
  each libc function the runtime calls to its own Linux-style handler,
  with darwin's flags, errno values, `stat` and `sockaddr` layouts, and
  `fcntl` (`F_FULLFSYNC` is an fsync, `F_DUPFD_CLOEXEC` a dup), and gives
  `os.ReadDir` simulated directory streams. A direct system call through
  `syscall.Syscall` with a trap number, which bbolt makes for `madvise`, is
  not simulated.
- darwin on amd64, and iOS, are not supported.
