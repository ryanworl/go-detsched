# go-detsched

A fork of Go for finding concurrency and fault-tolerance bugs. With
`GOEXPERIMENT=detsched`, a program whose concurrency runs inside
`testing/synctest` bubbles is deterministic: one seed gives one schedule,
every time, and bubbles never wait in real time. The `detsched` command
uses that to search a package's tests for schedules, and with the SDK's
simulated cluster for faults, in which they fail. For each failure it
prints a command that replays it exactly, also under a debugger.

```
$ detsched ./pkg
FAIL 174 of 489 runs: panic: computed answer twice [recovered, repanicked]
	log:    /tmp/detsched-334332647/fail-1.log (and 9 more in /tmp/detsched-334332647)
	replay: cd /home/me/pkg && GODETSEED=1 GODETBRANCH=0 /tmp/detsched-334332647/pkg.test
```

It is Go 1.27.4 with the experiment added. Without the experiment it is
stock Go. It is for testing only: under the experiment all randomness,
`crypto/rand` included, comes from the seed. It runs on Linux (amd64,
arm64) and macOS (arm64).

**[Read the guide](/misc/detsched/README.md)** for how to use it,
[delve.md](/misc/detsched/delve.md) for debugging a replay (also in a
`-gcflags='all=-N -l'` build, which replays the same schedule), and
[skills/testing-with-detsched](/misc/detsched/skills/testing-with-detsched)
for an agent skill.

## Install

Download a release for your platform, `linux-amd64`, `linux-arm64` or
`darwin-arm64`, from the
[releases](https://github.com/ryanworl/go-detsched/releases) page.
Each archive unpacks to a directory named after its version:

```
v=go1.27.4-detsched39
mkdir -p ~/sdk
curl -fsSL https://github.com/ryanworl/go-detsched/releases/download/$v/$v.linux-amd64.tar.gz | tar -C ~/sdk -xz
export PATH=$HOME/sdk/$v/bin:$PATH
go version    # go version go1.27.4-detsched39 linux/amd64
```

The archive's `bin` holds `go`, `gofmt`, `detsched`, and Delve's `dlv`,
which the `debug:` lines detsched prints run. Its dlv is patched for
replays: positions on every stop, going back with checkpoints, cheap
hit-count breakpoints, and JSON output (see
[delve.md](/misc/detsched/delve.md)).

`SHA256SUMS` in each release lists the archives' checksums. The macOS
binaries are not notarized. Download them with curl, as above, or remove
the quarantine that a browser adds before you unpack the archive:
`xattr -d com.apple.quarantine go1.27.4-detsched39.darwin-arm64.tar.gz`.

To build from source instead, with any Go 1.24.6 or later installed:

```
git clone https://github.com/ryanworl/go-detsched && cd go-detsched
misc/release/build.bash
export PATH=$PWD/bin:$PATH
misc/release/delve.bash bin/dlv   # the archives' dlv: Delve 1.27.2 with misc/release/delve/*.patch
```

The SDK (assertions, fault sites, the simulated cluster) is an ordinary
module; take the version that matches your toolchain, v0.N.0 for
go1.27.4-detschedN. Tests that use it still build and pass with stock Go:

```
go get github.com/ryanworl/go-detsched/misc/detsched@v0.39.0
```

## Versions

A release is named after the Go release it is based on and a release
number that only grows: `go1.27.4-detsched1`, `go1.27.4-detsched2`, and
after a move to Go 1.27.5, `go1.27.5-detsched3`. The toolchain never
switches to another Go toolchain (`GOTOOLCHAIN=local` in `go.env`), since a
stock toolchain has no detsched experiment, so a module whose `go` line asks
for a newer Go than the release fails to build with a clear error instead.

## This repository

The repository is Go's source tree at the release named in `VERSION`, plus
the experiment: changes behind `goexperiment.DetSched` in `src`, and
`misc/detsched` (the command, the SDK, their tests and docs).
[Go's own README](/README.md) and the rest of its documentation are
unchanged, except that Go's SECURITY.md and CONTRIBUTING.md, which
describe the Go project's processes, are left out. Each commit here is a
published snapshot of the development tree; see
[misc/release](/misc/release/README.md) for how releases are made.

Go is distributed under the BSD-style license in [LICENSE](/LICENSE), and
so is this fork.
