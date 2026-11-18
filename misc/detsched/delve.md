# Debugging detsched failures with Delve

A failing run under `GOEXPERIMENT=detsched` replays exactly from the line
`detsched` prints for it, so you can stop it in
[Delve](https://github.com/go-delve/delve) as often as you like and the
run you see is the run that failed. The release bundles a Delve that
knows it debugs a replay:

- every stop and tracepoint line ends with the run's position,
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
  ([Stop at the Nth hit for free](#stop-at-the-nth-hit-for-free));
- `dlv --json` prints one JSON object per line ([JSON output](#json-output));
- `exit` runs the program to its end, so that it prints its trace line;
- file locations match by import path, and hits of inlined functions name
  both functions ([Breakpoints that hold](#breakpoints-that-hold)).

## Install Delve

The release has Delve 1.27.2 in `bin/`, next to `go` and `detsched`,
built with the patches in `misc/delve/` (its license is in
`misc/delve/LICENSE`). From a source tree, run
`misc/release/delve.bash bin/dlv` after `misc/release/build.bash`. On
macOS, Delve uses `debugserver` from the Xcode command line tools. A stock
Delve 1.27.2 or later debugs replays too, without the additions; see
[Stock Delve](#stock-delve).

## Run a replay under Delve

Run the `debug:` line `detsched` printed under the failure's `replay:`
line. It is the replay line with `dlv exec --disable-aslr` before the
binary and `--` before the program's arguments (the log's first line has
only the replay line; make that edit yourself when you start from it).
Delve's own flags go right after the binary, before `--`. Run it as
printed otherwise: see [Replaying a failure](README.md#replaying-a-failure)
for what each part does and what changes the schedule. In short, keep
`GODETBRANCH` (Delve does not follow forks), rebuild nothing but as below,
and add no arguments.

`--disable-aslr` is required on macOS: without it the program runs
itself again without ASLR at its start, Delve's breakpoints stay in the
image it replaced, and the program says so
(`detsched: running again without ASLR, so a debugger loses its
breakpoints; ...`). It does nothing on Linux.

For agents and long sessions, put the commands in a file and run Delve
without a terminal, saving what it prints:

```
cd /abs/pkg && GODETSEED=7 GODETBRANCH=10 dlv exec --disable-aslr /tmp/detsched-1234/pkg.test \
  -r stdout:prog.out -r stderr:prog.err --init cmds.dlv --allow-non-terminal-interactive=true --json \
  -- -test.run=TestX </dev/null >dlv.out 2>&1
jq -c 'select(.event != "output" and .event != "ok")' dlv.out
```

- `-r` keeps the program's own output, which may be megabytes, out of
  `dlv.out`, which then holds only Delve's records.
- A command that fails prints an `error` record (`cmds.dlv:LINE: ERROR`
  in text) and the file goes on.
- Delve exits at the end of the file, so put one `continue` per
  breakpoint stop you want, or use tracepoints, which need one.
  `goroutines`, `stack` and `print` after the last `continue` read the
  state where the run stopped.
- At `exit`, or the end of the file, Delve runs the program to its end
  with every breakpoint cleared, so its last record is the exit with the
  trace line, `{"event":"exit","status":1,"trace":"detsched: trace=0x...
  switches=... syncs=..."}` (in text, `Process N has exited with status 1
  (detsched: trace=...)`). It must equal the log's. `exit -k` kills the
  program instead. A program that does not end within 5 minutes is
  killed, with a message that it printed no trace line.

Nothing you do in Delve changes the run: its stops run no program code,
and time spent stopped does not exist for the program's virtual clock.
Delve's `call` would run code; it is refused on optimized functions, and
must not be used in the build below.

## Read locals: replay a build without optimizations

The kept binary is optimized: Delve warns `debugging optimized function`
at every stop, locals are often unreadable
(`could not find loclist entry`), and `next` and `step` do not go line by
line. A build of the same source with `-gcflags='all=-N -l'` runs the same
schedule and prints the same trace line, because the compiler counts
ticks and synchronization points where the source has them and garbage
collection never changes the schedule. In it, Delve prints every local,
steps line by line, and a condition can name any variable in scope.

`detsched` builds one in `<outdir>/debug/` while it searches and runs
seed 1 in it; when the schedules match, it prints
`detsched: debug lines run BIN from now on`, and the `debug:` lines
printed after that and in the summary run it. When they don't, the
summary's fallbacks say why
([Debug lines run the optimized binary](README.md#debug-lines-run-the-optimized-binary)).
With `-exec`, or to build one yourself, use the same toolchain and source,
in the package's directory, and put it in place of the binary on the
`debug:` line, changing nothing else:

```
cd /abs/pkg && GOEXPERIMENT=detsched CGO_ENABLED=0 go test -c -gcflags='all=-N -l' -o /tmp/pkg.debug.test .
cd /abs/pkg && GODETSEED=7 GODETBRANCH=10 GODETSEARCH=trace=0x5a28688c3de0741d dlv exec --disable-aslr /tmp/pkg.debug.test
```

(`go build -gcflags='all=-N -l'` for a command without tests;
`-gcflags='-N -l'`, for the package's own code only, works too and runs
faster.) The line's `GODETSEARCH=trace=` checks the run: a build that runs
another schedule prints `detsched: warning: this run's schedule is not
the one its replay line was printed for` at exit. Switch and tick numbers
are the same in both builds, so `GODETSTOP` and `restart` points carry
over. When the schedules part, find where by comparing the two builds'
switch lines ([Find where two runs diverge](#find-where-two-runs-diverge)),
then debug the optimized binary as below.

## Stop at the failure

```
break fail runtime.detAssertFailed
continue
stack 20
frame 5 print h.state
goroutines -t 30
```

- `runtime.detAssertFailed` stops at every failing assertion, including a
  failed compose check or branch; `frame N` moves to your harness's frame.
- `testing.(*common).Fail` stops at `t.Error` and `t.Fatal`.
- Panics and fatal errors stop by themselves (`unrecovered-panic`,
  `runtime-fatal-throw`); a panic on a sim node with `OnPanic` set does
  not, so break on `sim.onPanic` for it.
- A run that hangs: [attach to it](#attach-to-a-spinning-or-hung-run).

## Where the run is

Every stop, tracepoint and return line ends with the run's position, as
`GODETSTOP`'s stop lines write it:

```
> [s] example.com/kv/server.(*Store).apply() ./store.go:40 (hits goroutine(149):1 total:2) (PC: 0x5ef6d3) at t1.078676089 s2307 k93341 node server (start 2)
```

- `t1.078676089`: the goroutine's virtual time in seconds, as the fault
  timeline counts them; `t-` outside every synctest bubble.
- `s2307`: the goroutine switches so far, as `GODEBUG=dettrace=2`
  numbers them.
- `k93341`: the tick, the run's finest clock: one per entry of a function
  of user code that calls something or loops, inlined or not, and one
  per loop iteration. A breakpoint at a function's entry stops just
  before that entry's tick: `GODETSTOP=k93341` stops before it, and
  `k93342` in it.
- `node server (start 2)`: the sim node the goroutine runs on, and which
  start of it; nothing for no node.

In expressions, for `print`, `on` and `cond`: `detsched.time` (seconds,
-1 outside a bubble), `detsched.tick`, `detsched.switches` and
`detsched.node` (`""` for no node), also on system stacks.

`goroutines` shows each goroutine's node and whether the node crashed or
is paused. Goroutines of a crashed node are frozen where they were, so
read the live ones first. `goroutines -with node server` lists every start
of node `server`, `-with node "server (start 3)"` one start; `-without
node` and `-group node` work too. A filter Delve does not know answers
with the filters it takes.

A failing run that uses `sdk/sim` has the fault timeline at the end of its
log, in the same seconds as `t`:

```
sim: 9.408758544 server: power failure
sim: 9.408758544 nemesis: start kill server
sim: 9.620196967 server: start
```

Read it before breaking on a crash, a restart, or a disk fault: it says
what happened and when without a debugger. Virtual time moves only while
every goroutine of the bubble waits on a timer, so order events by `s` and
`k`, which always move.

## Stop at a point of the run

Every hit of a breakpoint traps into Delve, also when its condition is
false: about 1ms on Linux, 13ms on an arm64 Mac. A tracepoint on
`time.Now` turns a 0.04s replay of `testdata/simcompose` into 37s, and
with a condition that is never true, 29s. So never condition a breakpoint
on time to look at a window: run to the window's start first, then set
breakpoints. (Hit counts are the exception; see
[Stop at the Nth hit for free](#stop-at-the-nth-hit-for-free).)

A point is `t9.4` (virtual seconds), `s12345` (a switch) or `k812345678`
(a tick); the position on any stop or tracepoint line is one. `restart`
with a point runs the replay again from its start to that point at native
speed, with breakpoints disabled on the way, and stops in the code of the
goroutine running there. It works after the program exited too.

```
(dlv) restart t25.38
> time.Now() .../src/time/time.go:1348 (PC: 0x4bd229) at t25.380001899 s4265 k128732
```

To end a window too, put `GODETSTOP` among the replay line's variables,
after its `cd`. It lists points, separated by commas; at the first tick
of a goroutine's own code at or after each, the program prints
`detsched: stop 1 at t10.025092087 s1822 k90267 goroutine 10`, calls
`runtime.detStop`, where Delve can break, and runs on unchanged:

```
cd /abs/pkg && GODETSTOP=t9.4,t10.2 GODETSEED=7 GODETBRANCH=10 dlv exec --disable-aslr /tmp/detsched-1234/pkg.test --init window.dlv ...
```

```
break stop runtime.detStop
continue
trace st example.com/kv/server.(*Store).apply
on st print s.lastIndex
continue
clearall
continue
```

The first `continue` reaches 9.4s with no trap, the second traces until
10.2s, the third runs to the end. Use `clearall`: `clear st` leaves the
breakpoints a tracepoint sets on the function's returns. At a stop,
`stack` shows `runtime.detStop`, `detStopFire` and `detPreempt` above the
goroutine's code (`restart` with a point steps out of them, unless the
goroutine yields there: see [Go back](#go-back)).

Without Delve, `GODETSTOP=t9.4:dump` prints every goroutine's stack at
9.4s, with each one's node and state, and runs on; several points answer
"what was everyone doing then" in one run.

## Go back

The run is the same every time, so going back is running it again to an
earlier point, as rr does with a recording, and every value there reads
as it did:

- `checkpoint [note]` records the stop (its tick, goroutine, instruction,
  and the breakpoints' hit counts); `checkpoints` lists them, and
  `clear-checkpoint cN` deletes one.
- `restart c1` runs to checkpoint c1 and stops at the same instruction of
  the same goroutine, with the same hit counts.
- `rewind` (`rw`) goes back to the previous breakpoint stop of the
  session (tracepoint hits don't count), or to the start; each `rewind`
  one further.
- `restart t9.4`, `restart s12345`, `restart k812345678` run to a point,
  as above. `restart` alone runs from the start with the breakpoints, as
  in stock Delve.

At a point that is the last tick of its goroutine's quantum, the
goroutine yields, and its code goes on only at a later switch. There
`restart` stays at `runtime.detDebugStop`, at the point's exact
position, and names the goroutine's frame; `next` or `continue` runs the
other goroutines first, as the replay does:

```
(dlv) restart s1725
> runtime.detDebugStop() .../src/runtime/detstop.go:141 (PC: 0x428520) at t5.699000000 s1725 k107530 node client (start 1)
goroutine 158 yields here, at the last tick of its quantum, so its code goes on only at a later switch; frame 3 is its code: net.(*netFD).connect.func2() .../src/net/fd_unix.go:94
(dlv) frame 3 locals
```

```
(dlv) break d runtime.detAssertFailed
(dlv) continue
> [d] runtime.detAssertFailed() .../src/runtime/detfeed.go:639 (hits goroutine(10):1 total:1) (PC: 0x418c0e) at t36.353801885 s7202 k375635
(dlv) checkpoint failed
(dlv) restart t25.38
(dlv) trace st example.com/kv/server.(*Store).apply
(dlv) continue
(dlv) restart c1
> runtime.detAssertFailed() .../src/runtime/detfeed.go:639 (PC: 0x418c0e) at t36.353801885 s7202 k375635
```

Each costs a replay up to the point: under a second for
`testdata/simcompose`.

## Breakpoints that hold

- Name every breakpoint, `break NAME LOCATION`, then `cond NAME ...` and
  `on NAME ...`: with numbers, one `break` that fails shifts the later
  numbers, and conditions attach to the wrong breakpoints.
- Prefer a function to a line: `break example.com/kv/server.(*Store).apply`,
  or a unique suffix, `break sim.(*disk).powerFail`. A closure is
  `pkg.F.func1`, and a closure in a closure `pkg.F.func1.1`, as stacks
  print them, inlined or not, with or without the import path.
- A line is `FILE:LINE`, matched as a path suffix: lengthen it when it is
  ambiguous (`fs.go:183` matches io/fs too; `sdk/sim/fs.go:183`), and
  include the module version for a dependency
  (`example.com/lib@v1.8.0/log.go:288`). A file also matches by its
  package's import path wherever it was built, so
  `github.com/acme/widgets/widgets.go:22` works for a module go.mod
  `replace`s with a local directory, and one command file serves builds
  from different checkouts.
- Read the line Delve prints when it sets a breakpoint. It names the
  function the address belongs to: a line in a closure lands in `F.func1`,
  a line of an inlined function in its caller. `could not find statement`
  means the line has no code; pick a nearby one. `(multiple functions)`
  means one breakpoint at each copy, such as a function inlined into its
  callers or a method promoted through an embedded struct into the
  compiler's wrappers. Hits then name both functions:
  `> goroutine(1): [a] main.(*base).Record(...) (inlined in main.(*list).Record)`
  (in JSON, `func` and `inlined_in`) is a hit of `(*base).Record`.
- An address stops for one breakpoint. A function inlined into another at
  its first statement shares its address, so after
  `trace a pkg.(*T).Free`, `trace b pkg.(*T).release` says
  `tracepoint b does not stop at 0x..., which tracepoint a has` and stops
  at release's other call sites; `on a stack 3` shows release at `a`'s
  stops. When `a` has all of `b`'s addresses, the error names it.
- A breakpoint that never stops proves nothing until a tracepoint on the
  function's entry has fired. For crash and disk-fault paths, read the
  fault timeline instead.

## Read values in optimized code

- Arguments and receivers are readable at a function's entry; locals
  often are not. Replay the build without optimizations, or break at the
  entry of the function that receives the value and walk fields from
  there (`print r.id`).
- A condition on an argument of an inlined function cannot be evaluated:
  break on the caller.
- A package variable takes its package's name when that is unique
  (`print sim.world`), else its import path in quotes
  (`print "github.com/ryanworl/go-detsched/misc/detsched/sdk/sim".world`).
- A type assertion takes the package's name (`print x.(*wire.Request)`),
  never a quoted path. It is rarely needed: `print x` shows an interface's
  dynamic type and value, and `print x.Field` reads a field of it.
- Expressions cannot call functions or methods; read fields. String
  comparisons work: `cond NAME c.name == "TestCompose"`.
- Strings print up to 1024 bytes (`print`, `on NAME print`, `args -v`,
  `locals -v`), 256 in a tracepoint's line. Longer values are cut: raise
  `config max-string-len 4096`, `config max-array-values 1000`, or
  `config max-variable-recurse 3`.
- `runtime.curg.goid` is the current goroutine's ID, the same in every
  replay: find the goroutine in one run, then
  `cond NAME runtime.curg.goid == 1354` in the next.
- A condition Delve cannot evaluate stops at every hit with
  `error evaluating expression`. Fix it before trusting the stops.

## Tracepoints

A tracepoint prints and continues, so one `continue` prints every hit.
Use it, not `break` with `on` actions, to log values: a breakpoint stops,
prints a source listing at every hit, and needs one `continue` per stop.

```
trace st example.com/kv/server.(*Store).apply
cond st detsched.node == "server (start 3)"
on st print runtime.curg.goid
on st print string(key)
on st stack 6
continue
```

Each hit prints `> goroutine(16): [st] ...apply(...) at t... s... k...`
and one line per `on` action (one expression each; a `[]byte` prints as
numbers, so `string(b)` for text). Lines starting with `>>` are returns,
printed only for a call whose entry printed, so they follow the
condition; an inlined copy has no return. A return line shows the
results, also in optimized code, read from the registers Go's ABI returns
them in, following one pointer, so an error shows its message:
`>> goroutine(1): [l] main.lookup => ("",error(*errors.errorString) *{s: "empty key"})`.
A result returned on the stack, or at a return that runs deferred calls
first, can stay unreadable; its value says why, and the build without
optimizations reads it. `breakpoints` lists each breakpoint with its hit
count.

### Stop at the Nth hit for free

`cond -hitcount NAME == 361` stops at the 361st hit, `>= 361` at every
hit from then on, `% 1000` at every 1000th: take the count from a
tracepoint in one replay, and stop at that hit in the next. The runtime
counts the hits before it, so they cost no trap: `cond -hitcount n ==
1000000` on `time.Now` in `testdata/simbusy` stops in 0.5s, where Delve's
counting took 14 minutes. `breakpoints` says
`(enabled, hits counted by the runtime)` when that applies:

- a breakpoint on a function, not a line inside it, in code that counts
  ticks (not the runtime, `os` or `syscall`) and calls something or loops,
  so that it has an entry tick;
- set at one address, not one per inlined copy;
- whose only condition is the total hit count (not `-per-g-hitcount`);
- at most 16 at once.

Otherwise Delve counts, at a trap per hit, and `breakpoints` says why:
`(enabled, hits counted by Delve, one trap each: it has a condition
besides the hit count)`; in JSON, `counted_by` is `runtime` or `delve`,
with the reason in `not_runtime_counted`. Either way the count and the
run are the same. Any other condition, `detsched.time` included, traps
every hit; for a point in time use `restart t9.4` or `GODETSTOP`.

## Find where two runs diverge

When a replay does not reproduce, two builds run different schedules, or
a change makes a failure disappear, find the first switch where two runs
part. `GODEBUG=dettrace=2` prints every goroutine switch and every jump of
a bubble's clock without changing the run. Export it: the replay line
starts with `cd`, so a variable written in front reaches only `cd`.

```
(export GODEBUG=dettrace=2; <replay line A>) 2>&1 >/dev/null | grep -a '^detsched: \(switch\|bubble\)' > a.trace
(export GODEBUG=dettrace=2; <replay line B>) 2>&1 >/dev/null | grep -a '^detsched: \(switch\|bubble\)' > b.trace
cmp a.trace b.trace; diff a.trace b.trace | head
```

The first differing `detsched: switch N to goroutine G at FUNC FILE:LINE`
line is where they part: FUNC is the innermost function, inlined or not,
and the line the call returns to, so an optimized build and one without
optimizations compare. When a `bubble ... time` line differs first, the
runs computed different amounts before it, which moved a node's clock.
To look there, `restart sN` in Delve, then `stack 20`.

## Attach to a spinning or hung run

A run that hangs usually spins: some timer keeps the virtual clock moving,
so it never deadlocks. Attaching does not change it, and the run goes on
unchanged after Delve detaches (Delve does not run an attached program to
its end at `exit`; answer `n` if asked to kill it):

```
printf 'goroutines -group node\ngoroutines -with startloc mypkg -t 30\nexit\n' > att.dlv
dlv attach "$(pgrep -n -x pkg.test)" --init att.dlv --allow-non-terminal-interactive=true </dev/null
```

`-t 30` shows 30 frames; the default 10 often hides whose goroutine it
is. A composer branch that spins is a forked child: rerun the replay with
its `GODETBRANCH` and attach to that. Without Delve, `kill -QUIT PID`
ends the run and prints every stack, each goroutine's header naming its
node and state (`[waiting, node 1 crashed]`).

## JSON output

`dlv --json` (or `config output json` in a session, `config output text`
back) prints everything as one JSON object per line. `cmd` and `src`
(`cmds.dlv:LINE`) name the command that printed it, and `event` says what
it is:

| event | printed by | fields besides `cmd` and `src` |
| --- | --- | --- |
| `trace` | a tracepoint hit | `bp`, `hits`, `goroutine`, `func`, `inlined_in`, `file`, `line`, `pc`, `pos`, and what its `on` actions ask: `args`, `vars` (`expr`, `value`), `frames` (`on NAME stack N`, as in `stack`), `locals`, `goroutine_info` |
| `return` | a tracepoint's return | the same, with `ret`; `bp` is the tracepoint's name |
| `break`, `watch` | a stop at a breakpoint or watchpoint | as `trace`; `watch` has `expr` |
| `stop` | another stop (`next`, `step`, `restart` to a point, `rewind`) | `goroutine`, `func`, `file`, `line`, `pc`, `pos` |
| `value` | `print` | `expr`, `type`, `value` |
| `args`, `locals` | `args`, `locals` | `vars`: `name`, `type`, `value` |
| `stack` | `stack` | `frames`: `n`, `func`, `file`, `line`, `pc` |
| `goroutines` | `goroutines` | `goroutines`: `id`, locations, `node`, `node_state`, `wait`, `thread`, `labels`, and `frames` with `-t N` |
| `breakpoints` | `breakpoints` | `breakpoints`: `id`, `name`, `kind`, `state`, `func`, `file`, `line`, `addrs`, `hits`, `cond`, `hitcond`, `counted_by` and `not_runtime_counted`, `actions` |
| `exit` | the program's exit | `pid`, `status`, `trace` |
| `error` | a command that failed | `error` |
| `output` | other text | `text` |
| `ok` | a command that printed nothing | |

`pos` is `{"t":"25.380001899","s":4265,"k":128779,"node":"server (start 3)"}`,
with `t` `"-"` outside a bubble and no `node` for no node.

## Stock Delve

A stock Delve 1.27.2 or later debugs replays the same way, without the
additions. Instead of each:

- positions: `print runtime.detTickEnd-int64(runtime.detTicks)` gives the
  tick, and `print runtime.curg.bubble.now` the time in nanoseconds since
  the Unix epoch (a bubble starts at 946684800000000000), which fails on a
  system stack;
- going back: record the tick of a hit, run again with `GODETSTOP=k` and
  that tick, break on the function at the stop, and `continue`;
- hit counts on hot functions: arm the breakpoint only between two
  `GODETSTOP` stops;
- the trace line: end the script with one more `continue`, since a stock
  `exit` kills the program first;
- locations: a file matches only by a suffix of its path on disk, and a
  closure only as `F.func1`, without its package; hits of an inlined
  function name only the function it was inlined into; strings are cut
  at 64 bytes, and results at a return are often
  `(unreadable empty OP stack)`;
- wait reasons it prints as `[unknown wait reason N]`: 13 semacquire,
  14 sleep, 18 select, 19 chan receive, 20 chan send, 21 sync.Cond.Wait,
  22 sync.Mutex.Lock, 23 sync.RWMutex.RLock, 24 sync.RWMutex.Lock,
  25 sync.WaitGroup.Wait, 40 synctest.Run, 41 synctest.Wait, 42 to 45 the
  durable chan receive, chan send, select and WaitGroup.Wait, 47 IO wait
  (simulated), 48 paused (simulated);
- JSON: filter the text, removing color escapes first. A tracepoint's hit
  starts with `> `, and each `on` print under it with a tab:

```
sed 's/\x1b\[[0-9;]*m//g' dlv.out | grep -a -v '^>>' | grep -a -E $'^> |^\t'
```
