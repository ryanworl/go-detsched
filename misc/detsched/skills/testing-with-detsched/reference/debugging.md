# Debugging a replay with dlv

Run dlv non-interactively from a command file and read its saved output. The human guide, `misc/detsched/delve.md` in the detsched repository, explains each step in more depth.

## Contents

- Run a replay under dlv
- Stop at the failure
- Breakpoints and expressions that work on the optimized binary
- Trace many hits
- Find where two runs diverge
- Attach to a hung run

## Run a replay under dlv

Take the replay line and put `dlv exec --disable-aslr` before the binary and `--` before its arguments, keeping everything else:

```
cd /abs/pkg && GODETSEED=N GODETBRANCH=K dlv exec --disable-aslr /tmp/detsched-XXXX/pkg.test --init cmds.dlv --allow-non-terminal-interactive=true -- -test.run=TestX </dev/null >dlv.out 2>&1
sed 's/\x1b\[[0-9;]*m//g' dlv.out | grep -a -v '^>>' | grep -a -E '^> |^\t|detsched: trace'
```

`--disable-aslr` is required on macOS: without it the program re-executes itself without ASLR at startup and none of dlv's breakpoints ever stops. The program then prints `detsched: running again without ASLR, so a debugger loses its breakpoints; start the program without ASLR (dlv exec --disable-aslr)` to stderr and runs to the end; rerun with the flag. It is harmless on Linux. The run under dlv is the same run: breakpoints, prints, and time spent stopped don't change it. Check that its trace line in `dlv.out` equals the log's. Never rebuild the binary for debugging, including with `-gcflags=all=-N -l`; work with the optimized binary.

In a command file, a failing command prints an error and the file goes on. dlv exits at the end of the file, so put one `continue` per breakpoint stop you want, or use tracepoints, which need a single `continue`.

## Stop at the failure

```
break fail runtime.detAssertFailed
continue
stack 20
frame 5 print h.state
goroutines -t 30
```

- `runtime.detAssertFailed`: every failing assertion, including a failed compose check or branch.
- `testing.(*common).Fail`: `t.Error` and `t.Fatal`.
- Panics and fatal errors stop by themselves.

## Breakpoints and expressions that work on the optimized binary

- Name every breakpoint (`break NAME LOCATION`, then `cond NAME` and `on NAME`): a numbered `break` that fails shifts the numbers of the later ones.
- Locations: `pkg/path.(*Type).Method`, a unique suffix such as `sim.(*disk).powerFail`, or `FILE:LINE` matched as a path suffix, with the module version for dependencies (`hashicorp/raft@v1.8.0/raft.go:288`). Read the `Breakpoint NAME set at ... for FUNC` line: `could not find statement` means pick another line, and `FUNC` may be a closure (`.func1`) or the caller of an inlined function.
- A breakpoint that never stops proves nothing until a tracepoint on the function's entry has fired. Read the fault timeline instead of breaking on crash and disk-fault paths.
- Read arguments and receivers at function entry; locals in optimized code are often unreadable. A condition on an argument of an inlined function can't be evaluated: break on the caller.
- Expressions: no function or method calls; a package variable takes its package's name when that is unique (`sim.world`), else the quoted import path (`"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim".world`); type assertions need the quoted path (`x.(*"github.com/org/mod/pkg".T)`); string comparison works; raise `config max-string-len 1000` for long values.
- A condition dlv can't evaluate stops at every hit with `error evaluating expression`. Fix it before trusting the stops.
- `runtime.curg.goid` is the current goroutine's ID, the same in every replay. `runtime.curg.bubble.now` is virtual time in nanoseconds; subtract 946684800000000000 to get the timeline's seconds.

## Trace many hits

```
trace st github.com/org/kv/server.(*Store).apply
cond st runtime.curg.bubble.now > 946684809000000000
on st print runtime.curg.goid
on st print s.lastIndex
on st stack 6
continue
```

One `on` per expression. Use `cond -hitcount NAME == 361` to stop at the 361st hit once a trace showed which hit matters, and `cond NAME runtime.curg.goid == G` once a trace showed which goroutine.

## Find where two runs diverge

```
(export GODEBUG=dettrace=2; <replay line A>) 2>&1 >/dev/null | grep -a '^detsched: \(switch\|bubble\)' > a.trace
(export GODEBUG=dettrace=2; <replay line B>) 2>&1 >/dev/null | grep -a '^detsched: \(switch\|bubble\)' > b.trace
cmp a.trace b.trace; diff a.trace b.trace | head
```

`dettrace=2` doesn't change the run. Export it: the replay line starts with `cd`, so a variable written in front of it reaches only `cd`. The first differing `switch N to goroutine G at FUNC+OFF` line is where the schedules part; a differing `bubble ... time` line first means the runs computed different amounts before it. To stop there: `break sw runtime.detTraceSwitch`, `cond sw pp.det[0].switches == N-1`, then `print gp.goid` and `goroutine G stack 20`.

## Attach to a hung run

A spinning run can be attached to without changing it, and goes on unchanged after dlv exits:

```
printf 'goroutines -group userloc\ngoroutines -with startloc mypkg -t 30\nexit\n' > att.dlv
dlv attach "$(pgrep -n -x pkg.test)" --init att.dlv --allow-non-terminal-interactive=true </dev/null
```

dlv prints wait reasons as `[unknown wait reason N]`: 14 sleep, 18 select, 19 chan receive, 20 chan send, 22 sync.Mutex.Lock, 23 RWMutex.RLock, 24 RWMutex.Lock, 25 WaitGroup.Wait, 41 synctest.Wait, 42 to 45 the durable forms, 47 IO wait (simulated), 48 paused (simulated).
