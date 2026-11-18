# Debugging detsched failures with Delve

A failing run under `GOEXPERIMENT=detsched` replays exactly from the
command `detsched` prints for it. You can stop the replay in
[Delve](https://github.com/go-delve/delve) as often as you like, at any
breakpoint, and the run you see is the run that failed. This guide shows
how to do that and lists the traps that cost time.

Every command here was run against `testdata/simcompose` (with
`SIMCOMPOSE_BUG=1`) and an unpublished hashicorp/raft harness, with
Delve 1.27.2, on linux/amd64 and darwin/arm64.

## Install Delve

The release archive has Delve 1.27.2 in `bin/`, next to `go` and
`detsched`, so the `debug:` lines run it once that directory is on your
`PATH` (Delve's license is in `misc/delve/LICENSE`). To use another
version, install it with any Go toolchain; it needs to be 1.27.2 or later
to read Go 1.27 binaries:

```
go install github.com/go-delve/delve/cmd/dlv@latest
```

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
stopped does not exist for it.

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

In Delve, `print runtime.curg.bubble.now` gives the current goroutine's
virtual time in nanoseconds. A synctest bubble starts at 2000-01-01 UTC,
which is 946684800000000000, and harnesses create the `World` as the
bubble starts, so 9.408758544 in the timeline is 946684809408758544 in
Delve. To look only at a window of the timeline, don't put that in a
breakpoint's condition: stop the run at the window's ends instead, as the
next section shows.

## Stop at a point of the run

Every hit of a breakpoint stops the process and switches to Delve, also
when its condition is false: about 1ms a hit on Linux, more on macOS.
On testdata/simcompose, a tracepoint on `time.Now` turns a 0.04s run into
37s, and the same tracepoint with a condition that is never true still
takes 29s. A condition on `runtime.curg.bubble.now` also fails with
`curg has no member bubble` at the first hit on a system stack, which
stops the script. Instead, let the run stop where you want to start
looking, and set breakpoints only then.

`GODETSTOP` lists points, separated by commas, at which the program
stops:

- `t9.4`: 9.4 seconds of virtual time, as the fault timeline counts them
  (since the bubble of the goroutine about to run started);
- `s12345`: switch 12345, as `GODEBUG=dettrace=2` numbers them;
- `k812345678`: tick 812345678. Ticks are the run's finest clock: the
  compiler counts one at every function entry and loop back edge in your
  code, so every hit of a breakpoint there has its own tick.

The program stops at the first tick, in a goroutine's own code, at or
after each point. It prints the position there, and calls
`runtime.detStop`, where Delve can break:

```
detsched: stop 1 at t10.025092087 s1822 k90267 goroutine 10
```

Then it runs on as if it had not stopped: `GODETSTOP` does not change the
run, and costs nothing without a debugger. Put it among the replay
line's variables, after the `cd`:

```
cd /home/me/kv && GODETSTOP=t9.4,t10.2 GODETSEED=7 GODETBRANCH=10 dlv exec --disable-aslr /tmp/detsched-1234/kv.test --init window.dlv ...
```

with a script that sets its breakpoints at the first stop and clears them
at the second:

```
break stop runtime.detStop
continue
trace cand github.com/hashicorp/raft.(*Raft).runCandidate
on cand print runtime.detTickEnd-int64(runtime.detTicks)
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
names.

### Go back

The run is the same every time, so going back is running it again to an
earlier point. `print runtime.detTickEnd-int64(runtime.detTicks)` at a
breakpoint gives the tick of the hit, as the `on` line above records for
every hit. Run again with `GODETSTOP=k` and that tick, break on the
function at the stop, and the next `continue` stops at that same hit.
Running again costs a replay up to the point, which runs at native speed.

### Look without a debugger

With `:dump` after a point, as in `GODETSTOP=t9.4:dump`, the program
prints the stack of every goroutine at the stop, as `kill -QUIT` would,
with each goroutine's node and state, and runs on. Several points answer
"what was everyone doing then" in one run, without Delve.

## What changes the schedule

These leave the run unchanged (its trace line stays the same):

- breakpoints, tracepoints, conditions, `print`, `stack`, `goroutines`,
  and staying stopped for any length of time;
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
- `runtime.curg` is the current goroutine: `runtime.curg.goid` its ID,
  `runtime.curg.bubble.now` its bubble's virtual time.

## Many hits: conditions and tracepoints

A tracepoint prints and continues on its own, so one `continue` runs the
whole replay and prints every hit:

```
trace pf sim.(*disk).powerFail
on pf print runtime.curg.bubble.now
trace cand github.com/hashicorp/raft.(*Raft).runCandidate
cond cand r.localID == "node-2"
on cand print runtime.curg.goid
on cand stack 6
continue
```

Each hit prints a line `> goroutine(16): [pf] github.com/ryanworl/go-detsched/misc/detsched/sdk/sim.(*disk).powerFail(...)`
followed by one line per `on` action; lines starting with `>>` are the
function's returns.

- `on` takes one expression; add one `on` line per value.
- Goroutine IDs are the same in every replay. Find the goroutine in one
  run, then condition on it in the next: `cond NAME runtime.curg.goid == 1354`.
- `cond -hitcount NAME == 361` stops at the 361st hit, and `> 1000` after
  it: count hits in one replay, stop at the right one in the next.
- String comparisons work: `cond NAME c.name == "TestCompose"`.
- A condition Delve cannot evaluate (a misspelled or optimized-out
  variable) stops at every hit with `error evaluating expression`. Fix the
  expression before relying on what the breakpoint shows.
- `breakpoints` lists each breakpoint with its hit count.
- Each hit costs about a millisecond, also with a false condition, so a
  breakpoint on a function that runs millions of times makes a replay
  take hours. Set it only between two stops (see
  [Stop at a point of the run](#stop-at-a-point-of-the-run)).

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
hides the frame that tells whose goroutine it is. Delve 1.27.2 does not
know Go 1.27's wait reasons and prints them as `[unknown wait reason N]`:

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
sed 's/\x1b\[[0-9;]*m//g' dlv.out | grep -a -v '^>>' | grep -a -E $'^> |^\t'
```

- A command that fails prints an error and the script goes on.
- Delve runs out of commands at the end of the file and exits; with
  breakpoints rather than tracepoints, add a `continue` per stop you want.
- `-r` sends the program's own output, including its trace line, to
  `prog.out` and `prog.err`, so that `dlv.out` holds only what Delve
  prints. Without it the two are interleaved in `dlv.out`, and a program
  that prints a lot buries the tracepoints.
- A tracepoint's hit is a line starting with `> `, and each `on` print
  under it starts with a tab, a real one: `$'...'` makes the shell put it
  in the pattern, as `grep -E` reads `\t` as a plain `t`. A printed
  struct takes several lines, each indented with tabs.
- `goroutines`, `stack`, and `print` after the last `continue` read the
  state where the run stopped.
