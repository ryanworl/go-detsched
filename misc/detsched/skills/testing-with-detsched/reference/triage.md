# Triaging a failure

The README's "Reading the summary" section says what each line of a group means, and its "Fallbacks" section how to fix each warning. Fix the fallbacks first: a failure of a program that ran two schedules for seed 1 may not replay.

## Read before debugging

1. The log's first lines: the replay line and the run's failures, each after `# `.
2. The first failure in the output, with its details.
3. The fault timeline at the end of the log (`sim: SECONDS NODE: EVENT`), around the failure's time: crashes, restarts, partitions, pauses, clock skips, disk faults, and the first failure of each assertion. For a branch failure, the branch's timeline comes first.
4. The group's `faults in its runs` line: the faults it likely needs.
5. The group's `also in N of these runs` lines: other kinds of failure in the same runs. They may be the same bug's symptoms or another bug; each has its own replay line.

Many failures explain themselves from these. For a run that timed out or hung, the log ends with every goroutine's stack; goroutines of a crashed node say so in their header and are frozen.

## SUT, harness, or detsched?

Decide from evidence, not from the message. Assume the harness until the evidence says otherwise: new harnesses have several bugs for each SUT bug.

- **Harness**: the SUT did what its documentation or code promises, and the check assumed more. Typical: a deadline shorter than the SUT's backoff, an operation that failed but took effect, a fault the check did not excuse, a race in the harness's own bookkeeping, a harness lock or callback that blocked a node. Evidence: dlv shows the SUT's state consistent with its contract.
- **SUT**: the SUT breaks its own invariant or documented guarantee. Evidence: dlv shows the inconsistent state and the code path that produced it; a flag check (the same binary and `-n`, with and without the harness flag that works around the bug) fails only with the flag.
- **detsched or sim**: the runtime or the simulation broke its own rules: a `fatal error:` in the runtime's `det*` code or in `sdk/sim`, a crashed node's goroutine that keeps running, behavior Linux would not show. Evidence: a small test in the style of `testdata` that fails without the SUT. Report it with the replay line and the kept binary.

## Signatures that are usually not SUT bugs

- A stall after a crash, or `a goroutine waits for a mutex that a crashed node holds`: a package variable or harness lock shared between nodes.
- Live goroutines in `chan send (durable)` or `chan receive (durable)` inside a lock that a crashed node's frozen goroutine holds: a lock made of a channel; use a `sync.Mutex`.
- A timeout with little output: a nemesis callback waiting for a node, a harness lock held by a paused node, or a slow checker.
- `concurrent map writes` in harness code; socket calls failing with `EPERM` (code on a goroutine of no node).
- `blocked goroutines remain`: a goroutine still running when the test body returned, which the summary names: an unjoined harness goroutine, or one the SUT's `Close` does not wait for.
- A failure only with a backward clock skip: a check comparing wall-clock times.
- `branch K died of signal 5`: a breakpoint in a forked branch; keep `GODETBRANCH=K`.
- A predicted deadlock no directed run failed: likely a false alarm.

## After classifying

- Harness: fix it, and judge the fix by a fresh search.
- SUT: put a workaround behind a flag so the search goes past the bug, confirm it with a flag check, and write down the replay line, the flag, and the root cause.
- detsched or sim: reduce it to a small test and report it.
