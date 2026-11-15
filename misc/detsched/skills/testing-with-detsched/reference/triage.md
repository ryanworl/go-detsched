# Reading and triaging a failure

## Contents

- What a failure gives you
- Does it replay?
- Is it the SUT, the harness, or detsched?
- Signatures that are usually not SUT bugs
- After classifying

## What a failure gives you

For each failure signature, `detsched` prints the path of the run's log, the replay line, and a `debug:` line that runs the replay under dlv, and says whether replaying it reproduced the failure. The log is the run's whole output:

- its first line, `# ` followed by the replay line;
- the test's output, every assertion failure (`detsched: assertion failed: MESSAGE DETAILS`), and the failing branch (`detsched: branch K (GODETBRANCH=K runs it in place)`);
- the trace line, `detsched: trace=0x... switches=N syncs=M`, which identifies the run;
- the fault timeline, after the output on stderr: `sim: SECONDS NODE: EVENT` lines, from `world: new` to `world: close`, with every crash, power failure, restart, partition, pause, clock skip, and disk fault, in seconds of virtual time;
- for a run stopped at the per-run timeout, every goroutine's stack. Goroutines of the simulation say their node and its state in the header, such as `goroutine 10 gp=0x... m=nil [waiting, node 1 crashed]:`.

Read the first assertion failure, then the timeline around its time, before anything else. Many failures explain themselves from those two.

## Does it replay?

First check `detsched`'s output for warnings, at the start and at the end. It runs seed 1 twice before searching and once more when the search ends, minutes later, and `detsched: warning: seed 1 ran two different schedules (...), so failures may not replay` means the program depends on something besides its seed. Two runs started at once read nearly the same clock, so the run at the end is the one that usually catches a clock read at init. That warning, and replays that don't reproduce, are the only signs: nothing reports reading the real clock outside a bubble (a `time.Now()` taken at package init or before `synctest.Test`) or real files. A missing warning doesn't prove the program deterministic. Fix the dependence before triaging anything else, since no failure of that binary replays reliably.

Variables of the environment are handled for you: `detsched` finds the ones the schedule depends on (a harness flag such as `SIMCOMPOSE_BUG=1`) and puts them on the replay line, then runs each line in a new shell's environment and prints the verdict next to the failure. `the replay reproduced the failure in a new shell` means the line is enough. `did not reproduce the failure: it ended with "..."` or `failed the same way, but ran another schedule (...)` means the run depends on something the line can't carry. `warning: seed 1 runs another schedule in a new shell` means it could not find which variables matter.

To check by hand, run the replay line as printed and compare its trace line with the log's. If they differ while the line, the binary, and the machine are the same, the program is not deterministic, or detsched has a bug. Find the first divergence with two `GODEBUG=dettrace=2` runs of the same line (see `debugging.md`; for the seed-1 warning, `GODETSEED=1 GODETBRANCH=0` on a binary built with `GOEXPERIMENT=detsched CGO_ENABLED=0 go test -c`, since `detsched` keeps none when no run failed) and look at what the goroutine at the first difference was doing.

## Is it the SUT, the harness, or detsched?

Decide with evidence from the replay, not from the message alone. Assume the harness until the evidence says otherwise: overnight, every project found several harness bugs for each SUT bug in its first hours.

- **Harness**: the SUT did what its documentation or code promises, and the check assumed more. Typical: a deadline shorter than the SUT's backoff, an operation that failed but took effect, a fault the check did not excuse (a clock skip, a pause, a message delivered late), a race in the harness's own bookkeeping, a driver compose killed that was still running, a harness lock or callback that blocked a node. Evidence: dlv shows the SUT's state consistent with its contract, or the SUT's source shows the behavior is intended.
- **SUT**: the SUT's own state breaks its own invariant or documented guarantee (an acknowledged write lost after only synced data was kept, two leaders in one term, a store that no longer opens). Evidence: dlv shows the inconsistent state and the code path that produced it. Strengthen it with a reproduction under stock Go when the trigger is simple, and with a flag check: the same binary over the same seeds, with and without the harness flag that works around the bug, should fail only with the flag.
- **detsched or sim**: the runtime or the simulation broke its own rules. Signs: a `fatal error:` whose message or stack is in the runtime's `det*` code or in `github.com/ryanworl/go-detsched/misc/detsched/sdk/sim`; a goroutine of a crashed node that keeps running and touches live state; `fatal error: fault` right after a node crash; behavior Linux would not show (a socket, file, or timer acting impossibly). Evidence: a small test in the style of the repository's `testdata` packages that fails the same way without the SUT. Report it with the replay line and the kept binary.

## Signatures that are usually not SUT bugs

- `a goroutine waits for a mutex that a crashed node holds` or a stall after a crash: process-global state shared between nodes (a package variable of the SUT or a dependency), or a harness lock shared between nodes.
- A run that times out after a crash with live goroutines in `chan send (durable)` or `chan receive (durable)` inside a lock, and a goroutine labeled `node N crashed` frozen in the same lock: a lock made of a channel, shared between nodes. A crashed node's goroutine runs on to release only a `sync.Mutex` or `RWMutex`, so replace the channel lock with a `sync.Mutex`, which under the experiment counts as durably blocked as upstream synctest's channel did.
- A run that ends at the per-run timeout with little output: a nemesis callback (`Event`, `Restart`) waiting for a node, a harness lock held by a paused node, or an oracle that is too slow (profile it). Read the stacks in the log.
- `concurrent map writes` in harness code: a harness map without a mutex.
- Socket calls failing with `EPERM`: code running on a goroutine of no node.
- synctest reporting a deadlock or blocked goroutines when the test ends: a harness goroutine not joined.
- A failure only in runs with a backward clock skip: a check that compares wall-clock times or durations without allowing for the skip.
- `branch K died of signal 5`: a debugger breakpoint hit inside a forked branch; replay with `GODETBRANCH=K`.

## After classifying

- Harness: fix it, then judge the fix by a fresh search. The old seed's schedule is gone with the rebuild.
- SUT: put a workaround behind a flag so the search goes on past the bug, confirm it with a flag check, and write down the replay line, the flag, and the root cause.
- detsched or sim: reduce it to a small test and report it. Work around it in the harness only if the search cannot go on otherwise, and say so.
