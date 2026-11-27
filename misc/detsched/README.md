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
v=go1.27.4-detsched40
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
	replay: cd /home/me/go/misc/detsched/testdata/bugcache && GODETSEED=1 GODETBRANCH=0 GODETSEARCH='trace=0x5a28688c3de0741d machine=p4096.f7ffdffff' /tmp/detsched-334332647/bugcache.test
	debug:  cd /home/me/go/misc/detsched/testdata/bugcache && GODETSEED=1 GODETBRANCH=0 GODETSEARCH='trace=0x5a28688c3de0741d machine=p4096.f7ffdffff' dlv exec --disable-aslr /tmp/detsched-334332647/debug/bugcache.test
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
  predicted deadlock), and what counts as a feature, or with `-seeds`,
  which seeds ran;
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
	faults: needs: sim.disk.fsyncfail (0 of 8 fail without it); not needed: nemesis kill (7 of 8), nemesis clockskip (6 of 8); 7 of 8 fail with every fault, in variants of its first run reseeded at setup's end (switch 412)
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
- `faults` says which of the faults that came with the failure it
  needs. Once the group's replay reproduces it, `detsched` runs, in the
  background, variants of the group's first run reseeded at setup's end
  (at its start without setup), then the same variants with one kind of
  fault turned off for the rest of the run, for the 6 kinds that fired
  in the most variants. It runs 8 variants, and more, up to 64, until 4
  fail. A kind without which a third as many variants fail, or fewer, is
  one the failure `needs`; one without which as many fail is `not
  needed`. Variants part at setup's end because a failure noticed long
  after its cause, such as a stall caught at a deadline, would fail in
  every variant reseeded later. When too few variants fail to compare,
  as for a failure that needs a fault only some runs enable, `detsched`
  turns each kind the first run injected off in that run itself, from
  its last branch point, and says which kinds it `needs` there (it
  passes without them); with none, the line says `not found` and how
  many variants failed. A search of `-n` runs waits for
  this before its summary; a search that ends otherwise says it was not
  found. Plain loss on a link (`Link.Drop`) counts as the fault
  `sim.net.loss`.
- For a deadlock at the end of a test (`blocked goroutines remain`), a
  line names the goroutines left, by where they wait and what created
  them.
- For a run that [did not finish](#runs-that-did-not-finish), a line
  names the goroutines its last switches went to, and where each is.
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
that does not reproduce is the other sign. A run cut short, at `-timeout`
or when memory ran low, prints the trace line of where it was stopped and
is not compared. The warning says how `detsched` started the two runs
differently, whether either left the deterministic subset, and where two
more runs of seed 1 with `GODEBUG=dettrace=2` first differ: the switch,
the goroutine each switched to, and where each stopped the goroutine
that ran before it. The out directory keeps a window of each run's trace,
the few thousand lines before the first difference and a few hundred
after, with its final trace line, in the files the warning names. Fix it before
triaging: see what the goroutine at the first difference was doing
([delve.md](delve.md#find-where-two-runs-diverge)).

### Seed 1 left the deterministic subset

The runtime reports, once per stack, each place where a program depends on
the OS or on real time: waiting for a real file or network connection to
become ready, waiting in a system call for 10 ms of real time (a call
whose thread runs, or waits for a CPU or a disk, keeps the run going
however long a loaded machine takes), starting a
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
This entry means it could not find them, as for a program that ranges
over `os.Environ()`: run replay lines from the shell that ran
`detsched`. In det mode `os.Environ()` leaves out the variables replay
lines, `dlv` and the shell set differently from the search's runs
(`GODET*`, `_` and `OLDPWD`), so from that shell the line replays the
run; `os.Getenv` still reads them. The program never sees `GODEBUG`'s
`det` settings: `os.Getenv("GODEBUG")` and `os.Environ()` leave them out,
and leave out `GODEBUG` when nothing else is left.

### Machine lacks CPU features

A binary replays a seed on every machine of its GOOS and GOARCH, as long
as nothing it reads about the machine changes the work its Go code does.
In det mode the standard library sees only the CPU features that every
x86-64-v3 machine (Haswell, any AMD Zen), and on arm64 every Neoverse N1,
Apple M1, and later, has, so it takes the same code paths, assembly or
Go, on all of them: AVX-512, SHA-NI, VAES, GFNI, ADX, and arm64's SHA512
and SHA3 are hidden (`GODEBUG=cpu.*` settings still apply). On Linux the
auxiliary vector is the same on every machine and kernel, in
`golang.org/x/sys/cpu`'s and `golang.org/x/sys/unix`'s view and in
`/proc/self/auxv`: the same entries in the same order, and HWCAP words
that keep only what the standard library chooses code by (on arm64 FP,
ASIMD, AES, PMULL, SHA1, SHA2, CRC32 and atomics, so not `HWCAP_CPUID`,
with which libraries would read the ID registers). `AT_MINSIGSTKSZ`,
which grows with the vector registers, is the same everywhere, and
`prctl`'s `PR_SVE_GET_VL` and `PR_SME_GET_VL` fail as on a CPU without
SVE or SME. `runtime.NumCPU()`
returns 4 on every machine, and `GOMAXPROCS` is 1. The runtime does not
read the memory size, and reads the kernel version only for work outside
the deterministic subset, such as copying real files.

Packages also read the machine in their `init`: `go.uber.org/automaxprocs`
reads the cgroup's CPU quota, memory-limit packages read `memory.max`,
`/proc/meminfo` or `sysinfo`, and others count the processors in
`/proc/cpuinfo`. On Linux, outside simulated nodes, det mode gives these
the same machine everywhere: four CPUs with the features it keeps, 16 GiB
of memory, and a container at the root of a cgroup v2 hierarchy with no
CPU or memory limit. Opening `/proc/cpuinfo`, `/proc/meminfo`,
`/proc/stat`, `/proc/loadavg`, `/proc/mounts`, `/proc/self/mountinfo`,
`/proc/self/mounts`, `/proc/self/cgroup`, `/proc/self/auxv`,
`/sys/devices/system/cpu/{online,possible,present}`, or the cgroup v2
files `cgroup.controllers`, `cpu.max`, `cpu.weight`,
`cpuset.cpus.effective`, `cpuset.mems.effective`, `memory.max`,
`memory.high`, `memory.low`, `memory.min`, `memory.swap.max` and
`pids.max` in `/sys/fs/cgroup` for reading gives a file with det mode's
contents, also through `/proc/thread-self` and `/proc/PID`. cgroup v1
files, such as `/sys/fs/cgroup/memory/memory.limit_in_bytes`, do not
exist, and `sysinfo` and `sched_getaffinity` report the same machine. So
automaxprocs leaves `GOMAXPROCS` as it is, and a memory-limit package
that falls back to the machine's memory sets its limit from 16 GiB. The
process's real limits are unchanged, so a program is not told about a
smaller real memory limit, and files these directories hold that are not
listed, such as `memory.current` or `/sys/devices/system/cpu/cpu0/`, are
the machine's own.

The container and the kernel are fixed the same way. A container's
runtime writes `/etc/hosts`, `/etc/resolv.conf` and `/etc/hostname` for
each container, and the first lookup of any name, even `localhost` in
`net.Listen("tcp", "localhost:0")`, reads the first two. In det mode,
outside simulated nodes, the host name is `localhost` (`os.Hostname`,
`uname`, `/etc/hostname`, `/proc/sys/kernel/hostname`), `/etc/hosts`
holds only the loopback names, and `uname`, `/proc/version` and
`/proc/sys/kernel/osrelease` report Linux 6.8.0. `/etc/resolv.conf`
stays the machine's own, so a lookup that reaches DNS works wherever it
would without det mode, and package `net` parses it without counting
ticks, so what it holds does not move the schedule, unless it is
missing, names no name server or says `options no-reload`, which changes
how often `net` reads it again.
Files are copied in Go rather than by `copy_file_range`, `splice` or
`sendfile`, which work on some file systems and kernels and not others.
`os.File.ReadDir`, `Readdir` and `Readdirnames` return a real
directory's entries sorted by name: the order the kernel gives them in
depends on the file system (ext4 hashes names with a seed of its own,
tmpfs orders them by when they were made), so code that stops at the
first entry it wants did different work on another machine. Code that
reads directories with `syscall.Getdents` or `golang.org/x/sys/unix`
still sees the kernel's order. The kernel's real release and the hard
limit of open files, which some code reads with `getrlimit`, stay
visible, and the machine ID names them.

This entry means the machine lacks one of the features kept, so the
standard library runs other code here: search and replay on machines
that have them. What det mode cannot hide is what code reads itself: the
`CPUID` instruction on x86 (as `golang.org/x/sys/cpu` and
`github.com/klauspost/cpuid` do), the ID registers on arm64 by code that
does not check `HWCAP_CPUID`, sysctl on macOS, files about the machine
other than those above, the page size (`os.Getpagesize`), and the
process ID. So replay lines carry the machine's ID (`machine=p4096.f...`:
page size, CPU features, the kernel's HWCAP words, the CPU's ID
registers or signature, the hard limit of open files, and the kernel's
release), and a replay that runs another schedule on
another machine says what differs, such as `CPU features only there:
avx512f`, `MIDR 0x410fd401 there, unknown here` or `kernel
6.10.14-linuxkit there, 6.1.0-28-cloud-arm64 here`. Only a replay line
checks this: comparing the trace lines of `-seeds` runs on two machines
does not, so replay a seed whose trace differs with its replay line on
the other machine to see what differs. To find the code that
differs, run the replay line on both machines with `GODETSTOP='s*100000'`
and compare the stop lines (see `GODETSTOP` under
[Replaying a failure](#replaying-a-failure)): the first that differs
brackets the switch, `GODETSTOP='sN*1000'` and then `kN*100` from the last
line that agrees narrow it to a tick, and `GODETSTOP=kN:dump` there
prints each machine's stacks, whose difference names the code. The
warning of a replay that runs another schedule says so too.

### Tests share a fork point

One process runs the package's tests one after another, and only the
first `lifecycle.SetupComplete` of a process is a fork point. When later
tests complete setup too, every run pays their setup again, under the
faults the first setup turned on. Search one test at a time with `-run`.

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

### Assertions never reached

`detsched` lists the `sdk/assert` calls with a constant message in the
packages of the program's module (with `-run`, those in the tests it
selects), and warns of those no run reached. An `Always` that no run
evaluates checks nothing; a `Reachable` no run reaches marks a path the
workload never took. A search with no failures says nothing about them.
An `Always(false, ...)` marks a branch no run should reach: write it as
`assert.Unreachable`, which the warning leaves out.

### Sometimes never true

A `Sometimes` assertion that runs reached but that never held marks a
state the search never got to, and nothing after that state was tested.
Make the workload reach it, or check that the condition can hold.

### Runs end before faults

Of the runs that set up a simulation, a quarter or more ended before it
injected any fault (counted without the runs whose nemesis drew a run
without faults, by design). Those runs tested only the fault-free path:
the workload ends too soon after setup. A Nemesis can start with a calm of
up to 30 seconds, and buggify faults fire on a small share of operations.

### Faults never injected

Faults the simulation could have injected in 20 runs or more and never
did (a nemesis kind its storms could pick, a buggify site that runs
reached), and faults the harness keeps out, with why: a nemesis without
`NemesisConfig.Restart` kills, crashes, and stops no node; partitions and
cut links need 2 nodes in `NemesisConfig.Nodes`; node faults need a node
that `NemesisConfig.Exclude` leaves. Turn off the faults the program does
not claim to survive with `NemesisConfig.Disable` or `buggify.Disable`,
which this entry then leaves out.

### Branch did not run

A composer timeline forks a branch for each terminal check. Where the
process cannot fork (see [Fork refused](#fork-refused)), the runtime prints
`detsched: branch K did not run: the process cannot fork now (REASON)`,
the branch's check does not run, and the branch keeps its number, so
`GODETBRANCH` still counts the branches after it right. The fix is the
reason's.

### Shared descriptor

Each simulated node's file and socket descriptors are its own, as a
process's are, and so are each start's: a node's call on a descriptor
that another node opened, or an earlier start of the same node, fails
with `EBADF`, and the first such call of a World prints
`sim: node B (start 1) called write on descriptor N, which node A (start
1) opened`, adding `before it crashed or stopped` for an earlier start's.
It comes from a harness that keeps an `*os.File`, a `net.Conn`, or a
client that holds one in a package variable or other state that several
nodes, or several starts of a node, reach. Such runs often pass, as code
ignores the error of a call such as `Close`, so the summary counts them
and gives the replay line of the first, whose output has the message.
Open files and connections in the code each node runs.

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
The warning names the first switch where the builds part, with each
build's tick and position in the goroutine that ran before it; when
nothing above explains it, report it as a detsched bug.

### Memory

While less than a quarter of the memory (of the machine, or of its
cgroup) is available, `detsched` closes its stopped programs and starts no
runs; below a tenth, it stops the run holding the most private memory,
with `SIGQUIT`, so that its log (`memory.log`, for the largest) shows
where it was, and counts it as neither pass nor failure. A seed 1 stopped
this way ends the search with its replay line. A run whose memory grows
without bound usually fails first as one that
[did not finish](#runs-that-did-not-finish); this fallback is for runs
that stay under that limit but together fill the machine, and for runs
before any passed: bound what they keep, or lower `-p`. On Linux the replay line of the
largest runs it in a subshell with `ulimit -v` set above the size it was
stopped at, so that it ends with `fatal error: out of memory` and its
stacks instead of exhausting the machine.

### Output cut

A run keeps at most 16 MB of its output, its first and last 8 MB, with a
line where the rest was cut. Print diagnostics once per run, after an
assertion failed (`assert.Failures() > 0`), not on every failure.

## Runs that did not finish

Every run after the first that passed runs with limits set from the
most any passing run went, and a run that passes one fails as one that
did not finish, at a switch that its replay line stops at too. A run
stopped at a limit that the runs that passed since it started have
raised runs again from its start with the raised limits, since the first
runs to pass are the short ones. Each way a run does not finish is a
group:

- `did not finish: livelock at t=...`: while virtual time stayed put, the
  run's goroutines blocked and woke each other (switches other than
  preemptions) 10 times more often than in any passing run, and at least
  100,000 times, such as a retry without a backoff while what it waits
  for needs time to pass.
- `did not finish: ran N× the longest passing run`: 20 times its switches
  (at least a million; a forked run counts from its fork) or its virtual
  time (at least an hour), such as a poll whose condition never comes
  true or a timer that keeps firing.
- `did not finish: memory`: a GC found 20 times more memory live (at
  least 320 MB) than any passing run held, such as a history the harness
  keeps forever. Its debug line stops at the same switch (`stopat=`), as
  the build without optimizations has another heap.
- `did not finish: still running when the search ended` or `at -timeout`:
  the run was stopped from outside, with `SIGQUIT`, for its stacks. When a
  search ends, only runs going on for 10 times as long as the slowest
  passing run (and at least 10 seconds), which the status line counts,
  are kept; shorter ones are dropped. Its replay line stops at the switch
  it was stopped at (`stopat=`). A run blocked in real time, outside the
  deterministic subset, stops there too: read its stacks.

The group's line `of its last N switches, ...` names the goroutines most
of the run's last switches went to, and the innermost function of the
code under test each is in (the tests' module and the modules it
replaces with a local directory), else the innermost one outside the
runtime. Switches to goroutines that have exited count together, with the
functions they started in. Its log ends with every goroutine's stack.

## Replaying a failure

Run the printed replay line as it is: its `cd`, every variable on it, the
kept binary, and its arguments. It sets:

- `GODETSEED`, the seed, which sets all randomness;
- `GODETBRANCH`, the composer branch the failure was in, run in the process
  itself rather than in a forked child, or 0 to run the timeline without
  forking any branch;
- `GODETSEARCH`, quoted, for a run forked from another (`tape=`, its fork
  points), directed at a deadlock (`yield=`), or one that did not finish
  (the limit it passed, such as `maxstuck=`, or `stopat=`), and the run's
  schedule hash (`trace=`): a replay
  whose schedule differs says so at exit
  (`detsched: warning: this run's schedule is not the one its replay line
  was printed for`);
- the variables of `detsched`'s environment the schedule depends on, and
  every other variable the program read, as the run had them, and unsets
  with `env -u` those it read that the run did not have and whose
  presence changes the schedule, so the line runs the same in any shell.
  Before the search, seed 1 runs with every variable it read but did
  not have set to `1`, and then with halves of them, to find those; a
  variable only later runs read is unset untested.

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
`GODETSTOP`, `GOTRACEBACK`, attaching a debugger, copying the binary, and
another machine of the same GOOS and GOARCH, loaded or not (see
[Machine lacks CPU features](#machine-lacks-cpu-features)), leave the run
unchanged; add such a variable just before `GODETSEED=`, after
`env -u NAME`.

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
prints every goroutine's stack there and runs on. Each stop prints
`detsched: stop 1 at t9.400000000 s1822 k90267 goroutine 10 in
main.worker (main.go:42) trace=0x...`, with where the goroutine is and
the hash of the schedule so far, and a point followed by `*M` stops again
every M seconds, switches or ticks. To find where two runs that should
agree part, such as a replay on two machines, run both with
`GODETSTOP=s*100000` and compare the stop lines: the first that differs
brackets the switch, and `GODETSTOP=s1200000*1000` from the last line
that agrees, then `kN*M`, narrow it to the switch and the tick; when
one goroutine ran different code within its quantum, as package
initialization can, the `kN*M` lines differ in where it is, and
`GODETSTOP=kN:dump` prints its stack.

## Flags

All are optional.

- `-run regexp` runs only the matching tests, as `go test -run` does.
- `-duration d` stops after `d`; `-n N` stops after N runs, so that a CI job
  checks the same runs every time. Either then runs seed 1 once more
  before the summary; a signal skips that run.
- `-timeout d` (default 10m) stops a run that takes longer, with
  `SIGQUIT`, so that its log has every goroutine's stack, and counts it as
  a failure that [did not finish](#runs-that-did-not-finish).
- `-p n` (default: one fewer than the CPUs) runs `n` programs at once.
  Lower it when runs take enough memory to make the machine swap.
  `detsched` and its runs run at nice 10, so a search yields the CPUs to
  the rest of the machine's work.
- `-seed-base n` adds `n` to every seed after seed 1, so that machines
  splitting a search run disjoint seeds: shard `i` of a search with
  `-seed-base $((i << 32))` runs seeds `i<<32+2`, `i<<32+3`, and on.
- `-seeds` runs seeds `-seed-base`+1, +2, and on, each once from its
  start, instead of searching: no run is forked from a stopped program,
  and none is directed at a predicted deadlock. Use it for a failure rate
  that compares across commits, or for failures that each replay from
  their seed alone (`GODETSEED`, with the variables the replay line
  sets). A composer timeline still forks its branches, as it does when
  the binary runs on its own. Everything else is as in a search: `-p`,
  `-timeout`, `-duration`, `-n` (exactly `n` seeds), `-exec`, the
  variables replay lines set and unset, the `fail-*.log` files, the
  groups with their replay and debug lines checked in a new shell, the
  summary, which says which seeds ran, and `-json`, whose `start` has
  `"mode":"seeds"`. Seed 1 still runs first, as in a search, to check its
  schedule and find the variables it reads, but it is not one of the
  runs.
- `-exec binary` runs a prebuilt binary in the current directory instead
  of building a package, so that one job builds it and every shard runs
  the same file. `detsched` runs a copy it keeps, which replay lines
  name, and debug lines run it too, with optimizations. Build it as
  `detsched` does, in the package's directory:
  `GOEXPERIMENT=detsched CGO_ENABLED=0 go test -c -o pkg.test`. To
  compare two versions of the code by their failure rates over the same
  `-n`, search the package with each instead, giving one its own go.mod
  (`GOFLAGS=-modfile=go.before.mod detsched -n 3000 .`), so that debug
  lines keep the build without optimizations.
- `-json file` also writes the search's events to `file`, one JSON
  object per line, for a program such as a CI job to read; see
  [JSON output](#json-output). The text output stays the same.
- `-check` checks that the tests can fail; see
  [Checking that the tests can fail](#checking-that-the-tests-can-fail).

## JSON output

`-json file` writes one JSON object per line to `file` as the search
goes, alongside the text output, which does not change. Every object has
`event`, its kind, and `time`, when it was written (RFC 3339, UTC).
Times and durations are in seconds, in fields ending in `_s`. A wrapper
checks `"schema":1` on the `start` event: the schema's number changes
when a field changes its meaning or goes away, and new fields and events
may come without a change, so read the fields you know and ignore the
rest.

The events come in this order: `start`, then a `run` for every run as it
is recorded, then at the end, a `fallback` for each fallback, a `group`
for each group, and `end`, which is always the last. If `detsched` fails
before the search starts, such as when the package does not build, the
file has only `end`, with `exit_status` 2 and `error`.

```json
{"event":"start","time":"...","schema":1,"detsched":"go1.27.4-detsched40","mode":"search","package":".","binary":"/tmp/detsched-123/store.test","binary_sha256":"8aef...","cwd":"/src/store","args":["-test.run=^TestStore$"],"run":"^TestStore$","p":7,"seed_base":0,"n":1000,"timeout_s":600,"out":"/tmp/detsched-123"}
{"event":"run","time":"...","seed":7,"tape":"45:7,51:9","arm":"cell","forked":true,"result":"fail","exit_code":1,"trace":"0x180bd2a3779a224","switches":948814,"syncs":27753685,"machine":"p4096.f7ffdffff.h1f8bfbff.g2.s606a6","elapsed_s":41.2,"signature":"detsched: assertion failed: ...","branch":0,"group":1,"log":"/tmp/detsched-123/fail-1-1.log"}
{"event":"fallback","time":"...","kind":"seed-1-ran-two-schedules","what":"...","cost":"...","fix":"...","docs":".../README.md#seed-1-ran-two-schedules"}
{"event":"group","time":"...","id":1,"signature":"...","message":"...","details":"...","runs":147,"logs":["..."],"first":{"seed":7,"tape":"45:7,51:9","branch":0,"log":"..."},"replay":{"cwd":"/src/store","set":{"GODETSEED":"7","GODETBRANCH":"0","GODETSEARCH":"tape=45:7,51:9 trace=0x180bd2a3779a224 machine=..."},"unset":["NO_COLOR"],"argv":["/tmp/detsched-123/store.test","-test.run=^TestStore$"]},"replay_line":"cd /src/store && env -u NO_COLOR GODETSEED=7 ...","debug":{...,"argv":["dlv","exec","--disable-aslr","/tmp/detsched-123/debug/store.test","--","-test.run=^TestStore$"]},"debug_line":"...","reproduced":true,"replay_status":"reproduced","also":[{"signature":"...","runs":14,"log":"...","replay":{...}}],"faults":"needs: sim.disk.fsyncfail ..."}
{"event":"end","time":"...","runs":1000,"passed":853,"failed":147,"stopped":0,"signatures":1,"features":422,"elapsed_s":1200.4,"first_failure":{"run":6,"after_s":13.3},"slowest_pass_s":51.4,"slowest_fail_s":48.0,"exit_status":1}
```

- `start`: `detsched`'s version; `mode`, `search`, or `seeds` with
  `-seeds`; the package
  argument (or the `-exec` binary), the binary the runs run and its
  SHA-256, the directory they run in, and its arguments; the flags
  (`n` and `duration_s` only when set); and `out`, the directory of the
  logs and the summary.
- `run`: one per run the counts count, passing ones included, in the
  order the runs ended. `seed` is `GODETSEED`; `tape`, the branch points
  of a run forked from a stopped program (`forked`), from which its
  schedule departs; `yield`, the sites a run directed at a predicted
  deadlock yields at; `arm`, how it started (`fresh`, `cell`, or
  `directed`, see [the search](#the-search)). `result` is `pass`,
  `fail`, or `stopped`, for a run stopped because memory ran low, which
  counts as neither. `trace`, `switches`, and `syncs` are from the run's
  first trace line, which is that of a composer branch whose failure
  came first; two machines that run the same seeds of the same binary
  compare them, with `machine`, the machine ID the runtime reports. A
  failing run has its `signature`, the composer `branch` it failed in
  (0 for the timeline), its `group`, and its `log` if kept.
- `fallback`: `kind` is the anchor of its section under
  [Fallbacks](#fallbacks), which does not change; `what`, `cost`, and
  `fix` are the summary's text.
- `group`: in the order of the text's groups, with the same counts, logs,
  and `faults`, `left`, and `where` lines. `replay` is the command the
  text's replay line runs, from which `detsched` writes that line
  (`replay_line`), so they always agree: run `argv` in `cwd`, with the
  variables in `unset` removed from the environment and those in `set`
  set. When `memory_limit_kib` is there, the line caps the run's address
  space with `ulimit -v` first. `debug` is the same for the debug line.
  `reproduced` says whether the replay reproduced the failure in a new
  shell, and `replay_status` says what it did otherwise, as the text's
  `WARNING`. `also` has the group's other kinds of failure, each with the
  replay of its first run.
- `end`: the totals (`stopped` are the runs memory ran out in), the run
  and time of the first failure, the slowest passing and failing runs,
  `exit_status`, the status `detsched` exits with (0 if no run failed, 1
  if some run did, 2 for an error), and `error`, the error that ended
  the search, if one did. A second interrupt kills `detsched` before it
  writes `end`.

## Checking that the tests can fail

A search without failures shows that the tests found nothing, not that
they could. `detsched -check ./pkg` plants one mistake at a time in the
code under test and searches each such mutant:

```
detsched -check: caught 6 of 12 mistakes planted in code the passing runs reached, with 100 runs each (1 more did not compile and was skipped)
	survived: internal/store/store.go:36 drop the Lock and Unlock of c.mu
	caught:   internal/store/store.go:65 ignore the error in err != nil: detsched: assertion failed: the log counts the syncs that succeeded
```

- Each mistake is named by its file's path in its module and the line.
- It first searches the unmutated program with the same runs, and stops
  if any run fails: fix those failures first, since a mutant's could not
  be told from them.
- The code under test is the non-test Go files that the test binary
  links of the tests' module and of the modules its go.mod replaces with
  a local directory, such as a harness module's copy of the code it
  tests, but detsched's SDK. A coverage build's runs of
  seeds 1 to 8 pick the lines to mutate: only those a passing run reached.
- The mistakes: drop a `Lock` and its `Unlock`, drop every `Lock` and
  `Unlock` of one mutex across the package (one locked in one method and
  unlocked in another), buffer an unbuffered channel or unbuffer a
  buffered one, drop a `close`, drop a `Sync` call as if it succeeded,
  flip `<` to `<=` and the like, and ignore an `err != nil` check whose
  error path a passing run took. Up to 20 that compile, spread over these
  kinds, concurrency and durability first; one that does not compile,
  such as a variable left unused, is skipped.
- Each mutant runs `-n` runs (default 100) and stops at its first
  failure; `-run`, `-p` and `-timeout` apply as to a search. The mutants
  are built through `go build -overlay`, so the source is untouched.
- It exits with status 1 when a mutant survived. A survivor is a mistake
  the tests cannot see: make the workload run that code concurrently and
  under faults, and assert what it must keep. A survivor that breaks
  nothing the code promises, such as a buffer it does not depend on, needs
  nothing. A dropped lock around code with no channel operation, atomic,
  or other lock inside often survives: detsched switches goroutines mostly
  at those.

## The search

Each seed runs another schedule: the runtime perturbs the order in which
goroutines run, choosing for each seed between a random walk and PCT
(probabilistic concurrency testing) and their parameters. If the program
reports the end of its setup (`lifecycle.SetupComplete`), `detsched` runs
setup once in each of a few seeds (8 at a time, each forked 32 times),
stops each process right after its own setup, and forks it, each fork
reseeded there, so fresh seeds don't pay for setup and draw everything
from setup's end on anew. On
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
and release N's version is v0.N.0, so with go1.27.4-detsched40:

```
go get github.com/ryanworl/go-detsched/misc/detsched@v0.40.0
```

Without the experiment they do nothing, or print, so tests build and pass
with stock Go, except `sim.NewWorld`, which panics: start a test that
uses `sim` or `compose` with
`if !sim.Supported() { t.Skip("requires GOEXPERIMENT=detsched") }`.
`go doc -all` on each documents its API.

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

Under the experiment, `t.Fatal`, `t.FailNow`, `t.Skip`, and `t.SkipNow` on
a goroutine other than the test's fail the run at the call, which a line
before `fatal error: detsched: t.FailNow or t.SkipNow on a goroutine that
is not the test's` names: in stock Go they end only that goroutine, so
the test goes on, or waits for it for good, and `go vet` does not see the
goroutines that `Node.Go` starts. Send the error to the test's goroutine,
or check with `assert`.

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
- **Disk.** A node's disk starts with `/tmp` and the host's
  `os.TempDir()` (on macOS a per-user directory under `/var/folders`), so
  `os.CreateTemp("", ...)` works on every node. Writes stay in a page
  cache until fsync. With an ordered
  journal's semantics, fsync of any file commits the namespace. A killed
  node loses everything unsynced. `mmap` of a file is coherent: a write or
  truncation through a descriptor shows through its mappings at once,
  also after a failed fsync; on Linux the shared mappings of a file all
  map one memfd, so a store through one shows through the others at once,
  and on macOS each is a copy. Stores to a shared writable mapping reach
  the page cache before any call on one of the file's descriptors, and on
  `msync` (with `MS_SYNC`, an fsync), `munmap`, `mremap`, and a crash.
  Preallocation (`fallocate`, `fcntl(F_PREALLOCATE)` on macOS, a growing
  truncation) takes no memory until written.
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
- **Clock rates.** Each node's clock runs at its own rate relative to the
  simulation's, so nodes drift apart while each stays consistent with
  itself: `time.Now`, monotonic readings, timers, tickers, sleeps,
  context and socket deadlines, file times, and the node's TCP timeouts
  (connecting, keepalives, and giving up on data that cannot get through)
  all run on it, while network latencies and disk speeds are physical.
  Clocks run together during setup; when setup completes, each node
  draws a rate within ±1% of 1 (from the fault stream, so runs branched
  after setup draw their own), as does a node added later; the drift is
  reported as the fault `fault: sim.clock.drift`, which the summary can
  say a group needs. `Node.SetClockRate(r)` sets a node's rate
  from then on (1 keeps it with the simulation's), and its timers keep
  their times on its clock. A node's clock and rate stay across restarts.
  The rates are in the timeline (`n1: clock rate 1.00374`), and
  `testdata/simdrift` shows a lease that breaks under drift.
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
- **No file watchers.** On a node, creating a watcher of file system
  changes (`inotify_init1` on Linux, `kqueue` on macOS, as
  `fsnotify.NewWatcher` does) fails with `ENOSYS`, and the first one
  prints why: its events would come from the real file system. Notice
  changes by polling on nodes, or turn the watcher off in the harness.
- **Descriptors are per process.** A node's file and socket descriptors
  are its own and its start's, as a process's are: another node's call
  on one, or a later start's, fails with `EBADF`, and the first such call
  prints which node and start opened it and which used it in which call
  (see [Shared descriptor](#shared-descriptor)). Goroutines of no node,
  such as the test body, may use any node's, and a finalizer or cleanup
  runs on the node that set it.
- **Faults.** All of the simulation's choices come from the fault stream,
  which `GODETSEED` seeds. The simulation's own faults are buggify sites:
  `sim.net.retransmit`, `sim.net.reset`, `sim.net.udpdrop`, `udpdup`,
  `udpreorder`, `ecnce`, `ecnbleach`, `ecnmangle`, `sim.disk.fsyncfail`,
  `sim.disk.slowsync`, `sim.disk.readerror`, `sim.disk.enospc`,
  `sim.disk.partialcrash` (a crash keeps some unsynced writes),
  `sim.disk.torn` (sector by sector), and the opt-in
  `sim.disk.strictdirsync` (only directory fsyncs commit entries, which
  is reported when a power failure loses entries that a file's fsync
  since would have committed on a journaling disk),
  `sim.disk.bitflip`, and `sim.disk.misdirect`. They fire only once setup
  has completed (see [No faults](#no-faults)) and outside a nemesis's
  calms, and each one that fires is reported as the assertion
  `fault: SITE`. A link's `Drop` loss is reported as `fault: sim.net.loss`.
  The summary says which of them each group needs.
  `buggify.Disable` turns off a fault a system does not claim to survive.
- **Nemesis.** `World.NewNemesis` injects Antithesis's fault catalog, each
  fault for a random duration: partitions, clogs, and cut links in the
  three modes, restores, kills, process kills (three times as likely as
  power failures), graceful stops, pauses, throttles, clock skips, clock
  rates (one node's clock runs at 1/4 to 4 times the simulation's, to
  find timeouts measured on one node and enforced on another), and disk
  faults (syncs 10 to 1000 times as slow, or stalled until the fault
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
keeps the only P; after 10 ms, if its thread sleeps in the kernel waiting
for an event (a pipe, a socket, a child), a watchdog thread hands it off
so other goroutines can run. A call whose thread runs, or waits for a CPU
or a disk, keeps the P however long it takes, so a loaded machine does
not change the schedule. What the standard library reads about the
machine (CPU features, the number of CPUs) is fixed (see
[Machine lacks CPU features](#machine-lacks-cpu-features)). cgo callbacks
are not supported, and `go build -race` fails, because the race detector
randomizes the scheduler.

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
jumping instead; call `runtime.GC` there. A finalizer or cleanup set
in a bubble runs in a goroutine of that bubble, on the simulated node
that set it, so it may use the bubble's channels and timers, and the
bubble neither moves its clock nor reports a deadlock until it returns
(one that blocks for good is in the deadlock's report); one set on a
node whose process has since crashed or stopped never runs, as a dead
process runs none. Those set outside a bubble, or in one that has
ended, run outside every bubble, as upstream.

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
- Harness mistakes, which the summary must name: `runaway` (runs that
  do not finish), `hang` and `badharness`; and `mutate`, whose weak
  harness `-check` must find out.
- `delve`: the bundled Delve's tests on a failing replay of `simcompose`.

`go test` in `cmd/detsched` runs the corpus and checks the command itself,
in a few minutes on four cores.
