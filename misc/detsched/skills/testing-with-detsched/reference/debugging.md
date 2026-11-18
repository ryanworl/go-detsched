# Debugging a replay with dlv

Run dlv non-interactively from a command file and read its saved output, as JSON. The release archive has dlv 1.27.2 in `bin/`, next to `detsched`, built with detsched's patches: every stop and tracepoint line carries the run's position, `restart` goes back to a checkpoint or a point of the run, a hit count on a hot function costs nothing until its hit, and `--json` prints one JSON object per line. The human guide, `misc/detsched/delve.md` in the release archive, explains each step in more depth; its "Stock Delve" section says what to do with a dlv without the patches.

## Contents

- Run a replay under dlv
- Read the output as JSON
- Where the run is
- Stop at the failure
- Breakpoints and expressions that work on the optimized binary
- Go back: checkpoints, restart to a point, rewind
- Trace many hits, and stop at the Nth for free
- Find where two runs diverge
- Attach to a hung run

## Run a replay under dlv

Take the `debug:` line `detsched` prints under the `replay:` line. It is the replay line with `dlv exec --disable-aslr` before the binary and `--` before its arguments (the log's first line has only the replay line; make that edit when starting from the log). Add dlv's flags right after the binary, before `--`:

```
cd /abs/pkg && GODETSEED=N GODETBRANCH=K dlv exec --disable-aslr /tmp/detsched-XXXX/pkg.test -r stdout:prog.out -r stderr:prog.err --init cmds.dlv --allow-non-terminal-interactive=true --json -- -test.run=TestX </dev/null >dlv.out 2>&1
jq -c 'select(.event != "output" and .event != "ok")' dlv.out
```

`-r` keeps the program's own output, which may be megabytes, out of `dlv.out`, which then holds only dlv's records.

`--disable-aslr` is required on macOS: without it the program re-executes itself without ASLR at startup and none of dlv's breakpoints ever stops. The program then prints `detsched: running again without ASLR, so a debugger loses its breakpoints; start the program without ASLR (dlv exec --disable-aslr)` to stderr and runs to the end; rerun with the flag. It is harmless on Linux. The run under dlv is the same run: breakpoints, prints, checkpoints, restarts, and time spent stopped don't change it. Never rebuild the binary for debugging, including with `-gcflags=all=-N -l`; work with the optimized binary.

In a command file, a failing command prints an error (`{"event":"error",...}`, or `cmds.dlv:LINE: ERROR` in text) and the file goes on. dlv exits at the end of the file, so put one `continue` per breakpoint stop you want, or use tracepoints, which need a single `continue`. At `exit`, or at the end of the file, dlv runs the program to its end with every breakpoint cleared, so its last record is the exit with the trace line: `{"event":"exit","status":1,"trace":"detsched: trace=0x... switches=... syncs=..."}` (in text, `Process N has exited with status 1 (detsched: trace=...)`). It must equal the log's trace line (for a branch failure, the log's first one, followed by `detsched: branch K`); a replay that runs another schedule also prints `detsched: warning: this run's schedule is not the one its replay line was printed for`. `exit -k` kills the program instead, without a trace line.

## Read the output as JSON

With `--json`, every line of `dlv.out` is one record. `cmd` and `src` (`cmds.dlv:LINE`) name the command that printed it, and `event` says what it is:

- `trace` (a tracepoint hit), `return` (its return, with `ret`), `break` (a breakpoint stop), `watch`: `bp`, `hits`, `goroutine`, `func`, `file`, `line`, `pc`, `pos`, and what the breakpoint's `on` actions asked for: `args`, `vars` (one per `on NAME print EXPR`, with `expr` and `value`), `stack`, `locals`, `goroutine_info`;
- `stop`: any other stop (`next`, `step`, `restart` to a point, `rewind`);
- `value` (`print`: `expr`, `type`, `value`), `args` and `locals` (`vars`), `stack` (`frames`), `goroutines` (`goroutines`, each with `id`, locations, `node`, `node_state`, `wait`);
- `exit` (`status`, `trace`), `error` (`error`), `output` (other text, in `text`), `ok` (the command printed nothing).

`pos` is the run's position: `{"t":"25.380001899","s":4265,"k":128779,"node":"server (start 3)"}`. Without `--json`, `config output json` switches a session to it; `config output text` back.

## Where the run is

Every stop, tracepoint and return line ends with the run's position, as the runtime's `GODETSTOP` stop lines write it: ` at t25.380001899 s4265 k128779 node server (start 3)`. `t` is virtual time in seconds as the fault timeline counts them (`t-` outside a bubble), `s` the goroutine switches so far, `k` the tick (one per function entry and loop back edge in user code; a breakpoint at a function's entry is just before that entry's tick), and `node` the sim node the goroutine runs on and which start of it, absent for no node. In expressions, `detsched.time` (seconds, -1 outside a bubble), `detsched.tick`, `detsched.switches` and `detsched.node` (`""` for none) read the same, also on system stacks, where `runtime.curg.bubble.now` fails.

`goroutines` shows each goroutine's node and whether it crashed or is paused (goroutines of crashed nodes are frozen: read the live ones first). `goroutines -with node server` lists every start of node `server`, `goroutines -with node "server (start 3)"` one start; `-without node` and `-group node` work too. A wrong filter answers with the filters `-with` takes.

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
- Locations: `pkg/path.(*Type).Method`, a unique suffix such as `sim.(*disk).powerFail`, or `FILE:LINE` matched as a path suffix, with the module version for dependencies (`hashicorp/raft@v1.8.0/raft.go:288`). Read the `Breakpoint NAME set at ... for FUNC` line: `could not find statement` means pick another line, and `FUNC` may be a closure (`.func1`) or the caller of an inlined function; `for (multiple functions)() <multiple locations>` means one breakpoint at each place the function was inlined (detsched16 and earlier put some of them on a path that almost never runs; upgrade if such a tracepoint never fires), and each hit names the caller it was inlined into.
- A breakpoint that never stops proves nothing until a tracepoint on the function's entry has fired. Read the fault timeline instead of breaking on crash and disk-fault paths.
- Read arguments and receivers at function entry; locals in optimized code are often unreadable. A condition on an argument of an inlined function can't be evaluated: break on the caller.
- Expressions: no function or method calls; a package variable takes its package's name when that is unique (`sim.world`), else the quoted import path (`"github.com/ryanworl/go-detsched/misc/detsched/sdk/sim".world`); a type assertion takes the package's name (`x.(*pkg.T)`), never a quoted path, which does not parse there, and is rarely needed: `print x` shows an interface's dynamic type and value, and `x.Field` reads a field of the value inside; string comparison works; raise `config max-string-len 1000` for long values.
- A condition dlv can't evaluate stops at every hit with `error evaluating expression`. Fix it before trusting the stops.
- `runtime.curg.goid` is the current goroutine's ID, the same in every replay.

## Go back: checkpoints, restart to a point, rewind

The run is the same every time, so going back is running it again, at native speed with breakpoints disabled on the way, to an earlier point; the state there is the same, so every value reads as it did.

- `checkpoint [note]` records the stop (tick, goroutine, PC, hit counts); `checkpoints` lists them; `restart c1` runs to checkpoint c1 and stops at the same instruction with the same hit counts.
- `rewind` goes back to the previous breakpoint stop of the session (tracepoint hits don't count), or to the start if there is none; each `rewind` one further.
- `restart t9.4` (virtual seconds), `restart s12345` (switch), `restart k812345678` (tick) run to a point and stop where `GODETSTOP` does, in the code of the goroutine running there. The position of any stop or tracepoint line is a point.
- `restart` alone runs from the start with the breakpoints, as in stock dlv. All of these work after the program exited.

```
break d runtime.detAssertFailed
continue
checkpoint failed
restart t25.38
trace w github.com/org/kv/server.(*Store).apply
continue
restart c1
print s.lastIndex
```

Every breakpoint hit costs about 1ms (13ms on an arm64 Mac), also when its condition is false, so don't condition on the time to look at a window: `restart` to the window's start, then set breakpoints. To end a window too, put `GODETSTOP` in the replay line after its `cd`: it lists points, comma-separated (`GODETSTOP=t9.4,t10.2`); at the first tick at or after each, in a goroutine's own code, the program prints `detsched: stop I at tD sN kN goroutine G` and calls `runtime.detStop`, and runs on unchanged:

```
break stop runtime.detStop
continue
trace st github.com/org/kv/server.(*Store).apply
on st print s.lastIndex
continue
clearall
continue
```

The first `continue` reaches 9.4s with no trap, the second traces until 10.2s, the third runs to the end. Use `clearall`: `clear` leaves the breakpoints a tracepoint sets on the function's returns. Without dlv, `GODETSTOP=t9.4:dump` prints every goroutine's stack, with its node and state, at 9.4s and runs on.

## Trace many hits, and stop at the Nth for free

```
trace st github.com/org/kv/server.(*Store).apply
on st print runtime.curg.goid
on st print s.lastIndex
on st stack 6
continue
```

One `on` per expression. Once a trace showed which hit matters, `cond -hitcount NAME == 361` stops at the 361st hit (`>= N`, `> N` from then on, `% N` every Nth). The runtime counts those hits, so the hits before it cost no trap: `cond -hitcount n == 1000000` on `time.Now` stops in 0.5s where counting in dlv took 14 minutes. `breakpoints` says `(enabled, hits counted by the runtime)` when that applies: a breakpoint on a function (not a line inside it) in code that counts ticks (not the runtime, `os` or `syscall`), set at one address, whose only condition is the total hit count, at most 16 at once. Any other condition, `detsched.time` included, still traps every hit; `cond NAME runtime.curg.goid == G` once a trace showed which goroutine, or `cond NAME detsched.node == "server (start 3)"`, and keep it between stops when the function runs often.

## Find where two runs diverge

```
(export GODEBUG=dettrace=2; <replay line A>) 2>&1 >/dev/null | grep -a '^detsched: \(switch\|bubble\)' > a.trace
(export GODEBUG=dettrace=2; <replay line B>) 2>&1 >/dev/null | grep -a '^detsched: \(switch\|bubble\)' > b.trace
cmp a.trace b.trace; diff a.trace b.trace | head
```

`dettrace=2` doesn't change the run. Export it: the replay line starts with `cd`, so a variable written in front of it reaches only `cd`. The first differing `switch N to goroutine G at FUNC+OFF` line is where the schedules part; a differing `bubble ... time` line first means the runs computed different amounts before it. To stop there, `restart sN` in dlv, then `stack 20`.

## Attach to a hung run

A spinning run can be attached to without changing it, and goes on unchanged after dlv exits (dlv does not run an attached program to its end at `exit`):

```
printf 'goroutines -group node\ngoroutines -with startloc mypkg -t 30\nexit\n' > att.dlv
dlv attach "$(pgrep -n -x pkg.test)" --init att.dlv --allow-non-terminal-interactive=true </dev/null
```

The bundled dlv names the runtime's wait reasons, `IO wait (simulated)` and `paused (simulated)` included; a stock dlv prints them as `[unknown wait reason N]`.
