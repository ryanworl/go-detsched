# Debugging detsched failures with Delve

A failing run under `GOEXPERIMENT=detsched` replays exactly from the
command `detsched` prints for it. You can stop the replay in
[Delve](https://github.com/go-delve/delve) as often as you like, at any
breakpoint, and the run you see is the run that failed. This guide shows
how to do that and lists the traps that cost time.

Every command here was run against `testdata/simcompose` (with
`SIMCOMPOSE_BUG=1`) and an unpublished hashicorp/raft harness, with
the Delve the release bundles, on linux/amd64 and darwin/arm64.

In short, with the bundled Delve:

- every stop and every tracepoint line ends with the run's position,
  ` at t25.380001899 s4265 k128779 node server (start 3)`, and
  `detsched.time`, `detsched.tick`, `detsched.switches` and
  `detsched.node` read it in expressions
  ([Where the run is](#where-the-run-is));
- `goroutines -with node server` lists one node's goroutines;
- `restart t9.4`, `restart s4400` or `restart k128779` runs the replay
  again to a point, `checkpoint` and `restart c1` go back to a stop, and
  `rewind` to the previous one ([Go back](#go-back));
- `cond -hitcount NAME == 1000000` on a hot function stops at that hit in
  a fraction of a second, since the runtime counts the hits
  ([Many hits](#many-hits-conditions-and-tracepoints));
- `dlv --json` prints one JSON object per line, and `exit` lets the
  program print its trace line ([Script Delve](#script-delve)).

## Install Delve

The release archive has Delve 1.27.2 in `bin/`, next to `go` and
`detsched`, so the `debug:` lines run it once that directory is on your
`PATH` (Delve's license is in `misc/delve/LICENSE`). It is built with the
patches in `misc/delve/`, which add what this guide calls the bundled
Delve's: positions, going back, runtime-counted hit counts, JSON output,
and the trace line at exit, and fix a few bugs. To build it from a source
tree, run `misc/release/delve.bash bin/dlv` after `misc/release/build.bash`.

A stock Delve 1.27.2 or later also debugs replays, without those
features; the [Stock Delve](#stock-delve) section says what to do
instead.

On macOS, Delve uses `debugserver` from the Xcode command line tools.

## Replay first

For each failure, `detsched` prints the path of the run's log, a replay
line, and the same line under Delve:

```
	log:    /tmp/detsched-1234/fail-1.log
	replay: cd /home/me/kv && GODETSEED=7 GODETBRANCH=10 /tmp/detsched-1234/kv.test
	debug:  cd /home/me/kv && GODETSEED=7 GODETBRANCH=10 dlv exec --disable-aslr /tmp/detsched-1234/kv.test
```

Run the replay line as printed, with nothing added or removed. The run
prints a trace line at exit,

```
detsched: trace=0x5a28688c3de0741d switches=44900 syncs=1035987
```

and the replay reproduces when its trace line equals the one in the log.
When the failure was in a forked branch, the log has two: the branch's,
followed by `detsched: branch K (GODETBRANCH=K runs it in place)`, and
later the timeline's; the replay prints the branch's. A replay with
`GODETBRANCH=K` prints its branch's trace line and the `detsched: branch K`
line whether the branch fails or passes, so replays of two branches
compare directly. The log's first line is the replay line, prefixed with
`# `. If they differ, see
[What changes the schedule](#what-changes-the-schedule) and
[Where two runs diverge](#where-two-runs-diverge).

Every part of the line matters:

- `cd`: tests run in their package directory, and some read files there.
- `GODETSEED`: the seed. It sets all randomness, the schedule's and the
  program's.
- `GODETBRANCH`: which branch of the timeline to run. A test that uses
  `sdk/compose` forks the process to run its terminal checks on branches
  of the timeline. `GODETBRANCH=K` runs branch K in this process, with the
  schedule the forked child had, and skips the others; `GODETBRANCH=0`
  runs the timeline without forking any branch. Delve does not follow a
  fork, so always keep the variable: without it, a breakpoint inside a
  forked branch kills that branch with SIGTRAP, and the run reports
  `branch N died of signal 5`.
- `GODETSEARCH='...'`, on failures that `detsched`'s search found by
  forking from another run (`tape=`) or by holding goroutines at the lock
  sites of a predicted deadlock (`yield=`). Copy it as printed, quotes
  included.
- The binary: `detsched` keeps the binary that ran the failure next to the
  log. A rebuild of the package, even with only a log line added, gives
  another schedule for the same seed.
- The arguments, such as `-test.run=TestX`: adding or removing any,
  including `-test.v`, changes the schedule.

The line also sets the variables of your environment that the run's
schedule depends on, such as `SIMCOMPOSE_BUG=1` that a test reads to plant
a bug; `detsched` finds them itself. So the line works as printed in a new
terminal. `detsched` has already run it there, with `sh` in a new shell's
environment, and prints the verdict next to the failure:
`the replay reproduced the failure in a new shell`, or what differed, such
as `did not reproduce the failure: it ended with "..."` or
`failed the same way, but ran another schedule (...)`. A line that did not
reproduce for `detsched` will not reproduce for you; see
[Where two runs diverge](#where-two-runs-diverge). If `detsched` warns
that `seed 1 runs another schedule in a new shell`, it could not find
which variables the schedule depends on, and lines may need your
environment.

## Run it under Delve

Run the `debug:` line. It is the replay line with
`dlv exec --disable-aslr` in front of the binary and `--` in front of any
arguments:

```
cd /home/me/kv && GODETSEED=7 GODETBRANCH=10 dlv exec --disable-aslr /tmp/detsched-1234/kv.test
cd /home/me/kv && GODETSEED=7 GODETBRANCH=0 dlv exec --disable-aslr /tmp/detsched-1234/kv.test -- -test.run=TestCompose
```

The log's first line holds only the replay line; make that edit yourself
when you start from the log. Delve's own flags, such as `--init`, go right
after the binary, before `--`.

On macOS, `--disable-aslr` is required. A program built with the
experiment that finds itself loaded at a random address runs itself again
without ASLR, so that addresses, and with them map order, are the same in
every run; Delve keeps its breakpoints in the image the program replaced,
and none of them ever stops. The program says so on stderr when it
happens, then runs on and reproduces the same trace:

```
detsched: running again without ASLR, so a debugger loses its breakpoints; start the program without ASLR (dlv exec --disable-aslr)
```

On Linux the flag changes nothing; use the same command on both.

Set breakpoints, then `continue`. When the process exits, its trace line
is the same as without Delve, whatever you did on the way: Delve's stops
run no program code, and the program's clock is virtual, so time spent
stopped does not exist for it. Delve prints it in its exit message:

```
Process 130547 has exited with status 1 (detsched: trace=0x210a7c6e992190e7 switches=7924 syncs=34387)
```

`exit` while the program still runs, or the end of an `--init` script,
first runs it to its end with every breakpoint cleared, so that you see
the trace line, and says so; `exit -k` kills it instead. A program that
does not end within 5 minutes is killed with a message that it printed
no trace line.

The binary is optimized, and Delve says `Warning: debugging optimized
function` at every stop. Do not rebuild it with `-gcflags=all=-N -l` to
get rid of that: the rebuilt binary has another schedule, and the runtime
does not build that way under the experiment. Work with the optimized
binary as described below.

## Stop at the failure

- An assertion failure, including a failed compose check or branch:
  `break runtime.detAssertFailed`. Then `stack` shows the assertion's
  caller; `frame N` moves to your harness's frame to print its variables.
- A test failure (`t.Error`, `t.Fatal`): `break testing.(*common).Fail`.
- A panic or a fatal error: Delve stops at those by itself
  (`unrecovered-panic` and `runtime-fatal-throw`).
- A run that hangs: see
  [Attach to a spinning or hung run](#attach-to-a-spinning-or-hung-run).

## Where the run is

The bundled Delve ends every stop line, tracepoint line and return line
with the run's position, written as the runtime's `GODETSTOP` stop lines
write it:

```
> goroutine(16): [pf] github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.(*disk).powerFail(...) at t25.380001899 s4265 k128779
> [s] github.com/ryanworl/go-detsched/misc/detsched/testdata/simcompose.serve() ./simcompose_test.go:40 (hits goroutine(149):1 total:2) (PC: 0x5ef6d3) at t1.078676089 s2307 k93341 node server (start 2)
```

- `t25.380001899`: the goroutine's virtual time in seconds, as the fault
  timeline counts them; `t-` for a goroutine outside a synctest bubble.
- `s4265`: the goroutine switches so far, as `GODEBUG=dettrace=2` numbers
  them.
- `k128779`: the tick, the run's finest clock (see
  [Stop at a point of the run](#stop-at-a-point-of-the-run)). A
  breakpoint at a function's entry stops before the function's own tick:
  `GODETSTOP=k128779` stops just before that entry, and `k128780` in it.
- `node server (start 2)`: the simulated node the goroutine runs on, with
  which start of the node it belongs to; nothing for a goroutine on no
  node.

The same values are pseudo-variables of expressions, for `print`, `on`
and `cond`: `detsched.time` (seconds, -1 outside a bubble),
`detsched.tick`, `detsched.switches` and `detsched.node` (`""` on no
node). Unlike `runtime.curg.bubble.now`, they work on system stacks too.

`goroutines` shows each goroutine's node, and whether the node crashed or
is paused: goroutines of a crashed node are frozen where they were.

```
  Goroutine 42 - User: .../src/net/fd_unix.go:149 net.(*netFD).accept (0x5a30c8) [node server (start 3) crashed] [IO wait (simulated)]
```

`goroutines -with node server` lists the goroutines of every start of
node `server`, and `goroutines -with node "server (start 3)"` those of
one start; `-without node` and `-group node` work too. A filter Delve
does not know answers with the list of filters it takes.

## Read the fault timeline first

A failing run that uses `sdk/sim` prints the simulation's fault timeline
to standard error after its output, so the run's log holds it. Times are
seconds of virtual time since the `World` was created:

```
sim: 0.000000000 world: new
sim: 9.408758544 server: power failure
sim: 9.408758544 server: power failure keeps some unsynced sectors
sim: 9.408758544 nemesis: start kill server
sim: 9.620196967 server: start
sim: 19.800543570 nemesis: start clog stopped server out
```

Read it before setting any breakpoint on a crash, restart, or disk fault:
it answers "did the node lose power, and when" without a debugger.

Harnesses create the `World` as the bubble starts, so the timeline's
times are the `t` of the positions Delve prints, and `detsched.time` in
expressions: the power failure at 9.408758544 is at `t9.408758544`. (With
a stock Delve, `print runtime.curg.bubble.now` gives the time in
nanoseconds since the Unix epoch; a bubble starts at 2000-01-01 UTC,
946684800000000000.) To look only at a window of the timeline, don't put
the time in a breakpoint's condition, which costs a trap at every hit:
stop the run at the window's ends instead, as the next section shows.

## Stop at a point of the run

Every hit of a breakpoint stops the process and switches to Delve, also
when its condition is false: about 1ms a hit on Linux, more on macOS
(13ms on an arm64 Mac).
On testdata/simcompose, a tracepoint on `time.Now` turns a 0.04s run into
37s, and the same tracepoint with a condition that is never true still
takes 29s. So don't condition a breakpoint on the time to look at a
window of the run: let the run stop where you want to start looking, and
set breakpoints only then. (A hit count is the exception: see
[Many hits](#many-hits-conditions-and-tracepoints).)

A point of the run is one of

- `t9.4`: 9.4 seconds of virtual time, as the fault timeline counts them
  (since the bubble of the goroutine about to run started);
- `s12345`: switch 12345, as `GODEBUG=dettrace=2` numbers them;
- `k812345678`: tick 812345678. Ticks are the run's finest clock: the
  compiler counts one at every function entry and loop back edge in your
  code, so every hit of a breakpoint there has its own tick.

In the bundled Delve, `restart` with a point runs the replay again from
its start to that point, with the breakpoints disabled on the way, so
that it costs a replay at native speed:

```
(dlv) restart t25.38
> time.Now() .../src/time/time.go:1348 (PC: 0x4bd229) at t25.380001899 s4265 k128732
```

It stops where the runtime's `GODETSTOP` (below) stops, in the code of
the goroutine that runs there, and works after the program exited too.

`GODETSTOP` lists points, separated by commas, at which the program
stops without a debugger's help. The program stops at the first tick, in
a goroutine's own code, at or after each point. It prints the position
there, and calls `runtime.detStop`, where Delve can break:

```
detsched: stop 1 at t10.025092087 s1822 k90267 goroutine 10
```

Then it runs on as if it had not stopped: `GODETSTOP` does not change the
run, and costs nothing without a debugger. Put it among the replay
line's variables, after the `cd`. Two points make a window:

```
cd /home/me/kv && GODETSTOP=t9.4,t10.2 GODETSEED=7 GODETBRANCH=10 dlv exec --disable-aslr /tmp/detsched-1234/kv.test --init window.dlv ...
```

with a script that sets its breakpoints at the first stop and clears them
at the second:

```
break stop runtime.detStop
continue
trace cand github.com/hashicorp/raft.(*Raft).runCandidate
on cand print detsched.tick
continue
clearall
continue
```

The first `continue` runs to 9.4s without a trap, the second traces the
hits until 10.2s, and the third runs to the end without breakpoints. On
testdata/simcompose, tracing `time.Now` from 10s to 12s this way, 645
hits, takes 2.7s, and the two stops alone 0.15s. Use `clearall`, not
`clear cand`: a tracepoint also sets breakpoints on the function's
returns, which `clear` leaves behind.

At a stop, `stack` shows `runtime.detStop`, `runtime.detStopFire`, and
`runtime.detPreempt` on top of the code of the goroutine the stop line
names. (`restart` with a point steps out of them for you.)

### Go back

The run is the same every time, so going back is running it again to an
earlier point, as rr does with a recording. The bundled Delve has rr's
commands for it:

- `checkpoint [note]` records where the run is stopped: its tick, the
  goroutine and instruction, and the breakpoints' hit counts.
  `checkpoints` lists them, `clear-checkpoint cN` deletes one.
- `restart c1` runs the replay again to checkpoint c1, with the
  breakpoints disabled on the way, and stops at the same instruction of
  the same goroutine at the same tick, with the same hit counts, so that
  every value reads as it did.
- `rewind` (`rw`) goes back to the previous breakpoint stop of this
  session, or to the program's start when there is none. Each `rewind`
  goes one stop further back; tracepoint hits do not count as stops.
- `restart t9.4`, `restart s12345`, `restart k812345678` run to a point,
  as above; the position of any stop or tracepoint line is one.

```
(dlv) break d runtime.detAssertFailed
(dlv) continue
> [d] runtime.detAssertFailed() .../src/runtime/detfeed.go:639 (hits goroutine(10):1 total:1) (PC: 0x418c0e) at t36.353801885 s7202 k375635
(dlv) checkpoint failed
Checkpoint c1 created.
(dlv) restart t25.38
> time.Now() .../src/time/time.go:1348 (PC: 0x4bd229) at t25.380001899 s4265 k128732
(dlv) checkpoints
ID  When                                      Note
c1  t36.353801885 s7202 k375635 goroutine 10  failed
(dlv) restart c1
> runtime.detAssertFailed() .../src/runtime/detfeed.go:639 (PC: 0x418c0e) at t36.353801885 s7202 k375635
```

`restart` without arguments runs the program again from its start with
its breakpoints, as in stock Delve. Checkpoints last for the session.
Going back costs a replay up to the point, which runs at native speed:
under a second for testdata/simcompose.

With a stock Delve, record the tick of a hit with
`print runtime.detTickEnd-int64(runtime.detTicks)`, run again with
`GODETSTOP=k` and that tick, break on the function at the stop, and the
next `continue` stops at that same hit.

### Look without a debugger

With `:dump` after a point, as in `GODETSTOP=t9.4:dump`, the program
prints the stack of every goroutine at the stop, as `kill -QUIT` would,
with each goroutine's node and state, and runs on. Several points answer
"what was everyone doing then" in one run, without Delve.

## What changes the schedule

These leave the run unchanged (its trace line stays the same):

- breakpoints, tracepoints, conditions, `print`, `stack`, `goroutines`,
  and staying stopped for any length of time;
- hit counts the runtime counts, `checkpoint`, `restart` to a checkpoint
  or a point, and `rewind`;
- attaching Delve to a running process, and detaching;
- environment variables the program does not read;
- `GODEBUG=dettrace=2`;
- `GODETSTOP`, with or without `:dump`;
- `GOTRACEBACK`;
- copying the binary elsewhere.

These change it:

- rebuilding, for any reason;
- adding or removing arguments, including `-test.v` and `-test.run`;
- changing a variable the program reads, such as a harness's own flags
  (a variable read only after the failure, to print more, is fine);
- other GODEBUG keys: their names, not their values, change the run;
- printf debugging, which is a rebuild. Print from Delve instead, or add
  code that runs only after an assertion has failed (check
  `assert.Failures() > 0`) and accept that the binary is new: find a fresh
  failing seed with it.

The program runs one goroutine at a time and is preempted after a fixed
amount of compiled code, so anything that runs different code before the
failure moves every later preemption. Delve's `call` would run code; it is
refused anyway on optimized functions (`can not call optimized function`).

## Set breakpoints that hold

- Prefer a function to a line: `break pkg.(*T).Method`, with the package's
  import path, such as `break github.com/hashicorp/raft.(*Raft).runCandidate`.
  A short suffix works when it is unambiguous: `break sim.(*disk).powerFail`.
- A line is `FILE:LINE`, matched as a path suffix. Lengthen the suffix
  when the name is ambiguous (`fs.go:183` matches io/fs too; use
  `sdk/sim/fs.go:183`), and include the module version for a dependency:
  `break hashicorp/raft@v1.8.0/raft.go:288`.
- Read the line Delve prints when it sets a breakpoint. It names the
  function the address belongs to: a line inside a closure lands in
  `F.func1`, a line of an inlined function in its caller, and
  `(multiple functions)` means several copies. `could not find statement`
  means the line has no code (a declaration, a constant, a line the
  compiler folded into another); pick a nearby line with a statement.
- A breakpoint that never stops is a claim to check, not evidence that the
  code never runs. Once, a line breakpoint on the simulation's power
  failure path never hit, and an hour went to chasing a write-back bug
  that didn't exist; a breakpoint on the function, or the fault timeline,
  showed the power failure at once. Before trusting a silent breakpoint,
  check that a tracepoint on the function's entry fires.
- Name your breakpoints: `break NAME LOCATION`, then `cond NAME ...` and
  `on NAME ...`. With numbers, one `break` that fails shifts every later
  number, and the conditions attach to the wrong breakpoints.

## Read values in optimized code

- Arguments and receivers are readable at a function's entry; locals often
  are not (`could not find loclist entry`, `optimized out`). Break at the
  entry of the function that receives the value, or where a struct you
  want is passed on, and walk fields from there (`print r.localID`).
- A condition on an argument of an inlined function cannot be evaluated.
  Break on the caller instead.
- A package variable takes its package's name when no other package has
  that name, as in `print sim.world`, and otherwise its import path in
  quotes:
  `print "github.com/ryanworl/go-detsched/misc/detsched/sdk/sim".world`.
  An unquoted path does not parse (`expected operand, found 'go'`).
- Type assertions take the package's name, `print x.(*kmsg.OffsetCommitRequest)`;
  a quoted path does not parse there (`expected type, found
  "github.com/twmb/franz-go/pkg/kmsg"`). You rarely need one: Delve looks
  through an interface by itself, so `print x` shows the dynamic type and
  value, and `print x.Field` reads a field of the value inside.
- Expressions cannot call functions or methods. Read fields instead.
- Long values are cut; raise `config max-string-len 1000`,
  `config max-array-values 1000`, or `config max-variable-recurse 3`.
- `runtime.curg` is the current goroutine: `runtime.curg.goid` its ID.
  `detsched.time`, `detsched.node`, `detsched.tick` and
  `detsched.switches` give the run's position (see
  [Where the run is](#where-the-run-is)).

## Many hits: conditions and tracepoints

A tracepoint prints and continues on its own, so one `continue` runs the
whole replay and prints every hit:

```
trace pf sim.(*disk).powerFail
on pf print detsched.node
trace cand github.com/hashicorp/raft.(*Raft).runCandidate
cond cand r.localID == "node-2"
on cand print runtime.curg.goid
on cand stack 6
continue
```

Each hit prints a line
`> goroutine(16): [pf] github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.(*disk).powerFail(...) at t25.380001899 s4265 k128779`
followed by one line per `on` action; lines starting with `>>` are the
function's returns.

- `on` takes one expression; add one `on` line per value.
- Goroutine IDs are the same in every replay. Find the goroutine in one
  run, then condition on it in the next: `cond NAME runtime.curg.goid == 1354`.
- String comparisons work: `cond NAME c.name == "TestCompose"`, and
  `cond NAME detsched.node == "server (start 3)"`.
- A condition Delve cannot evaluate (a misspelled or optimized-out
  variable) stops at every hit with `error evaluating expression`. Fix the
  expression before relying on what the breakpoint shows.
- `breakpoints` lists each breakpoint with its hit count.
- Each hit costs about a millisecond, also with a false condition, so a
  breakpoint on a function that runs millions of times makes a replay
  take hours. Set it only between two stops (see
  [Stop at a point of the run](#stop-at-a-point-of-the-run)), or stop at
  a hit by its number, below.

### Stop at the Nth hit for free

`cond -hitcount NAME == 361` stops at the 361st hit, `>= 361` or `> 360`
at every hit from then on, and `% 1000` at every 1000th: count hits in
one replay, or take a tracepoint's count, and stop at the right one in
the next. In the bundled Delve, the runtime counts those hits when it can,
so the hits before the one wanted cost nothing: the breakpoint is not
written in the program until the hit before it, and Delve does not stop.
`breakpoints` says when it does:

```
Breakpoint n (enabled, hits counted by the runtime) at 0x4bd20b for time.Now() .../src/time/time.go:1347 (0)
	cond -hitcount == 3796
```

The runtime counts a breakpoint's hits when

- it is on a function, as in `break n time.Now`, not on a line inside
  it, and the function is compiled code that counts ticks (code of your
  module and of the standard library outside the runtime, `os` and
  `syscall`);
- its hit condition is on the total count (not `-per-g-hitcount`), and it
  has no other condition;
- it sets one breakpoint, not one per inlined copy, and at most 16
  breakpoints are counted at once.

Otherwise it is an ordinary breakpoint, as in stock Delve. The count is
the same either way, and the run's schedule too. The per-goroutine hit
counts Delve prints count only the hits that stopped.

Stopping at `time.Now` == N, counted by the runtime and by Delve (a
condition `1 == 1` forces Delve's counting):

| Replay | N | Runtime | Delve |
| --- | --- | --- | --- |
| testdata/simcompose, linux/amd64 | 3796 | 0.29s | 4.6s |
| testdata/simbusy, linux/amd64 | 20000 | 0.51s | 20s |
| testdata/simbusy, linux/amd64 | 1000000 | 0.49s | 838s |
| testdata/simcompose, darwin/arm64 | 3000 | 0.30s | 39s |
| testdata/simbusy, darwin/arm64 | 20000 | 0.27s | 283s |

Both stop at the same tick. A condition on anything else, including
`detsched.time`, still costs a trap at every hit; for a point in time use
`restart t9.4` or `GODETSTOP`.

## Where two runs diverge

`detsched` runs seed 1 twice before it searches, and once more when the
search ends, minutes later in real time. When their schedules differ, it
prints

```
detsched: warning: seed 1 ran two different schedules ("detsched: trace=0x... switches=... syncs=...", then "detsched: trace=0x... ..."), so failures may not replay; the program depends on something besides its seed, such as real time or the OS
```

or, for the run at the end, the same warning with
`("detsched: trace=0x..." at the start of the search, "detsched: trace=0x..." at its end)`.
Read the end of the output too: two runs started at once read nearly the
same real clock and often run the same schedule, so the run at the end is
the one that usually catches a clock read at init.

That warning is the only report of such a dependence. In particular,
nothing reports a program reading the real clock outside a bubble, such
as a package variable set from `time.Now()` at init and later compared
with bubble time. The check sees a dependence only when it changes those
runs, so one that rarely changes a run can pass it; replays that do not
reproduce are then the sign. Fix the dependence before reading the
failures, since they will not replay. To find it, build the test binary
the way `detsched` does, which keeps none when no run failed,

```
cd /home/me/kv && GOEXPERIMENT=detsched CGO_ENABLED=0 go test -c -o /tmp/kv.test .
```

run `cd /home/me/kv && GODETSEED=1 GODETBRANCH=0 /tmp/kv.test` twice as
below, and look at what the goroutine at the first difference was doing.

When a replay does not reproduce, or a change makes a failure disappear,
find the first switch where two runs part. `GODEBUG=dettrace=2` prints
every goroutine switch and every jump of a bubble's clock, without
changing the run. Export it rather than putting it in front of the
replay line, which starts with `cd`:

```
(export GODEBUG=dettrace=2; <replay line>) 2>&1 >/dev/null | grep -a '^detsched: \(switch\|bubble\)' > a.trace
(export GODEBUG=dettrace=2; <the other run>) 2>&1 >/dev/null | grep -a '^detsched: \(switch\|bubble\)' > b.trace
cmp a.trace b.trace
diff a.trace b.trace | head
```

```
detsched: switch 12346 to goroutine 391 at runtime.gopark+0xec
detsched: bubble 1 time 946684804922035277
```

A `switch` line names the goroutine that runs next and where it resumes.
When the first difference is a `bubble ... time` line, the runs computed
different amounts before that point: busy time charges simulated time for
the work a node does, so the clock moved by different amounts before any
switch differed.

To stop at switch N in Delve, run with `GODETSTOP=sN` and break on
`runtime.detStop`:

```
break stop runtime.detStop
continue
stack 20
```

The stop line names the goroutine that runs, and the stack shows where
it is.

## Attach to a spinning or hung run

A run that hangs under detsched usually spins rather than sleeps: some
goroutine's timer keeps the virtual clock moving, so the run never
deadlocks and never ends. Attach to it:

```
pgrep -n -x kv.test
dlv attach PID
(dlv) goroutines -with startloc mypkg
(dlv) goroutines -group userloc
(dlv) goroutine 10 stack 30
(dlv) exit
```

Answer `n` when `exit` asks whether to kill the process; it then runs on
unchanged. A composer branch that spins is a forked child; rerun the
replay with its `GODETBRANCH` and attach to that process instead.

For stacks without a debugger, `kill -QUIT PID` ends the run and prints
every goroutine. A simulation's goroutines show their node and its state
in the header, such as
`goroutine 10 gp=0x... m=nil [waiting, node 1 crashed]:`; goroutines of
crashed nodes are frozen where they were, so read the live ones first.

`goroutines -t 30` shows 30 frames of each stack; the default 10 often
hides the frame that tells whose goroutine it is. The bundled Delve names
every wait reason of the detsched runtime; a stock Delve 1.27.2 does not
know Go 1.27's and prints them as `[unknown wait reason N]`:

| N | Wait reason |
| --- | --- |
| 13 | semacquire |
| 14 | sleep |
| 18 | select |
| 19 | chan receive |
| 20 | chan send |
| 21 | sync.Cond.Wait |
| 22 | sync.Mutex.Lock |
| 23 | sync.RWMutex.RLock |
| 24 | sync.RWMutex.Lock |
| 25 | sync.WaitGroup.Wait |
| 40 | synctest.Run |
| 41 | synctest.Wait |
| 42 | chan receive (durable) |
| 43 | chan send (durable) |
| 44 | select (durable) |
| 45 | sync.WaitGroup.Wait (durable) |
| 47 | IO wait (simulated) |
| 48 | paused (simulated) |

## Script Delve

For long sessions, and for agents, put the commands in a file and run
Delve without a terminal, saving everything it prints:

```
cd /home/me/kv && GODETSEED=7 GODETBRANCH=10 \
  dlv exec --disable-aslr /tmp/detsched-1234/kv.test -r stdout:prog.out -r stderr:prog.err --init cmds.dlv --allow-non-terminal-interactive=true </dev/null >dlv.out 2>&1
```

- A command that fails prints `cmds.dlv:LINE: ERROR` and the script goes
  on.
- Delve runs out of commands at the end of the file and exits; with
  breakpoints rather than tracepoints, add a `continue` per stop you want.
  The exit runs the program to its end with its breakpoints cleared, so
  the last line is `Process N has exited with status S (detsched:
  trace=...)`; compare it with the log's.
- `-r` sends the program's own output to `prog.out` and `prog.err`, so
  that `dlv.out` holds only what Delve prints. Without it the two are
  interleaved in `dlv.out`, and a program that prints a lot buries the
  tracepoints.
- Output that is not a terminal has no color escapes.
- `goroutines`, `stack`, and `print` after the last `continue` read the
  state where the run stopped.

### JSON output

`dlv --json` (or `config output json` in a session) prints everything
Delve prints as one JSON object per line, so a script reads it with `jq`
or a JSON parser instead of patterns over text. Each record has an
`event`, and `cmd` and `src` (`FILE:LINE`) name the command of the script
that printed it:

```
{"cmd":"continue","event":"trace","bp":"pf","goroutine":16,"hits":2,"func":"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.(*disk).powerFail","file":".../sdk/sim/fs.go","line":553,"pc":"0x5c39d3","pos":{"t":"25.380001899","s":4265,"k":128779},"args":[{"name":"d","type":"*.../sim.disk","value":"..."}],"vars":[{"expr":"detsched.time","type":"","value":"25.380001899"}],"src":"cmds.dlv:5"}
{"cmd":"continue","event":"break","bp":"d","goroutine":10,"hits":1,"func":"runtime.detAssertFailed","file":".../src/runtime/detfeed.go","line":639,"pc":"0x418c0e","pos":{"t":"36.353801885","s":7202,"k":375635},"src":"cmds.dlv:5"}
{"cmd":"args","event":"args","vars":[{"name":"msg","type":"string","value":"\"compose: anytime read-acked succeeds\""}],"src":"cmds.dlv:6"}
{"cmd":"print size","event":"error","error":"could not find symbol value for size","src":"cmds.dlv:8"}
{"cmd":"exit","event":"exit","pid":131544,"status":1,"trace":"detsched: trace=0x210a7c6e992190e7 switches=7924 syncs=34387","src":"cmds.dlv:10"}
```

| event | printed by | fields besides `cmd` and `src` |
| --- | --- | --- |
| `trace` | a tracepoint hit | `bp`, `hits`, `goroutine`, `func`, `file`, `line`, `pc`, `pos`, and `args`, `vars` (one per `on ... print`), `stack`, `locals`, `goroutine_info` as its `on` actions ask |
| `return` | a tracepoint's return | the same, with `ret` |
| `break` | a stop at a breakpoint | as `trace` |
| `watch` | a stop at a watchpoint | as `trace`, with `expr` |
| `stop` | a stop elsewhere (`next`, `step`, `restart` to a point, `rewind`) | `goroutine`, `func`, `file`, `line`, `pc`, `pos` |
| `value` | `print` | `expr`, `type`, `value` |
| `args`, `locals` | `args`, `locals` | `vars`: `name`, `type`, `value` |
| `stack` | `stack` | `frames`: `n`, `func`, `file`, `line`, `pc` |
| `goroutines` | `goroutines` | `goroutines`: `id`, `user`, `current`, `go`, `start` locations, `node`, `node_state`, `wait`, `thread`, `labels` |
| `exit` | the program's exit | `pid`, `status`, `trace` |
| `error` | a command that failed | `error` |
| `output` | any other text Delve prints | `text` |
| `ok` | a command that printed nothing | |

`pos` is the run's position: `t` (a string, `-` outside a bubble), `s`,
`k`, and `node` on a sim node. Every line of `dlv.out` is a record once
`-r` keeps the program's output out of it.

## Stock Delve

A stock Delve 1.27.2 or later debugs replays the same way, without the
bundled one's additions. Instead of each:

- positions: `print runtime.detTickEnd-int64(runtime.detTicks)` for the
  tick, `print runtime.curg.bubble.now` for the time in nanoseconds since
  the Unix epoch (a bubble starts at 946684800000000000), which fails on a
  system stack with `curg has no member bubble`;
- going back: run again with `GODETSTOP` (see [Go back](#go-back));
- hit counts on hot functions: arm the breakpoint only between two
  `GODETSTOP` stops;
- the trace line: end the script with one more `continue`, since a stock
  `exit` kills the program before it prints the line;
- JSON: filter the text, removing color escapes first:

```
sed 's/\x1b\[[0-9;]*m//g' dlv.out | grep -a -v '^>>' | grep -a -E $'^> |^\t'
```

  A tracepoint's hit is a line starting with `> `, and each `on` print
  under it starts with a tab; at a breakpoint stop, `print`, `stack` and
  `args` print lines that start without one, which this filter drops.
