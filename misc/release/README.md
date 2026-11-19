# Releasing detsched

The scripts here build, test, publish and release detsched, and take new
Go releases. They run on Linux and macOS (`-distpack` needs GNU tar, so
build archives on Linux).

## Versions

`VERSION`'s first line names the release that the development tree is
heading to, such as `go1.27.4-detsched1`: the Go release the tree is
based on, and a release number N that grows by one with every detsched
release and never restarts, so `go1.27.4-detsched2` may be followed by
`go1.27.5-detsched3`. `go version` and `go env GOVERSION` print it, and
binaries built with the experiment add ` X:detsched`. There is no dot
after the patch number because Delve reads such a version as go0.0 and
refuses to debug the binaries; `build.bash -distpack` and `publish.bash`
refuse a VERSION not of the form `goX.Y.Z-detschedN`.

The toolchain treats it as Go 1.27.4: a `go` line up to `go 1.27.4` builds.
`go.env` sets `GOTOOLCHAIN=local`, so a module that asks for a newer Go
fails with `go.mod requires go >= 1.27.5 (running go 1.27.4;
GOTOOLCHAIN=local)` rather than switching to a stock toolchain, which has
no experiment. `go mod init`, `go mod tidy` and `go get go@1.27.4` edit
go.mod as stock Go 1.27.4 does and write no `toolchain` line naming the
fork, and `go get toolchain@go1.27.4-detsched1` is refused, so go.mod files
stay usable with stock Go.

The SDK in `misc/detsched` is the module
`github.com/ryanworl/go-detsched/misc/detsched`. Release N's SDK is
tagged `misc/detsched/v0.N.0` (a subdirectory module's tags start with
its directory), and users take it with
`go get github.com/ryanworl/go-detsched/misc/detsched@v0.N.0`. The
runtime pushes its hooks to the SDK packages by that import path
(`//go:linkname` in `src/runtime/det*.go`), so a toolchain links only with
an SDK of the same path, and the SDK should match the toolchain's release.

## Build and test

```
misc/release/build.bash     # make.bash, then detsched into bin/
misc/release/gate.bash      # the checks every change must pass
```

`build.bash` takes make.bash's flags and environment (`GOROOT_BOOTSTRAP`,
`GOOS`, `GOARCH`). With `-distpack` it also writes
`pkg/distpack/$VERSION.$GOOS-$GOARCH.tar.gz`: make.bash's distribution
archive with `detsched` and Delve's `dlv` added to `bin/` (dlv at the
version the docs were tested with, with the patches in `delve/`, built by
the toolchain, which downloads its module, with its license and the
patches in `misc/delve/`) and its top
directory renamed to the version, so that unpacking it never mixes it into
a stock `go/`, plus a `.sha256` file. To build all three release archives:

```
for t in linux/amd64 linux/arm64 darwin/arm64; do
	GOOS=${t%/*} GOARCH=${t#*/} misc/release/build.bash -distpack
done
```

`gate.bash [GOROOT]` runs the detsched command's tests, the runtime,
testing/synctest, internal/godebug and sync tests with the experiment in
`-short` mode, checks that a stock hello world has no detsched
symbols, and, when `bin/dlv` exists, runs the bundled Delve's tests
(`misc/detsched/testdata/delve`) on a failing replay of
`testdata/simcompose`. `smoke.bash ARCHIVE` tests an archive on its own platform as a
user gets it: it checks the checksum, unpacks the archive in a new
directory, and with an empty environment checks `go version`, runs the
archive's own gate, runs `detsched -n 100` on the planted bug in
`testdata/bugcache`, and runs the replay line it prints under `env -i`,
which must fail with the same trace line, and the `debug:` line, which on
Linux must stop in dlv at the failure's panic.

## Delve

The release bundles Delve 1.27.2 with the patches in `delve/`, applied in
order by `delve.bash`, which `build.bash -distpack` runs. Each patch is a
commit message followed by its diff against the previous one, and says
whether it is a generic fix for upstream Delve or specific to detsched:

- `0001`: two generic fixes (module data of register variables, a
  tracepoint's stack printed twice);
- `0002`: generic: goroutine filter errors that say what is wrong, no
  color escapes off a terminal;
- `0003`: detsched: positions on every stop, `detsched.*` expressions,
  goroutines by node, `checkpoint`/`restart`/`rewind`, the trace line at
  `exit`, and the JSON output mode (generic but for its detsched fields);
- `0004`: detsched: hit counts counted by the runtime
  (`runtime.detDebug` in `src/runtime/detstop.go`);
- `0005`: generic: hit and stop lines name the inlined function and the
  one it was inlined into, and a tracepoint's returns follow its
  condition;
- `0006`: generic: `FILE:LINE` by the package's import path wherever it
  was built (a `replace`d module), and closures in closures
  (`pkg.F.func1.1`);
- `0007`: generic: a breakpoint whose addresses another one partly has
  is set and says which (hits name the function it was set on), return
  lines and records carry their tracepoint's name, and `goroutines -t`
  in JSON has each goroutine's frames;
- `0008`: generic but for its detsched part: a tracepoint's results read
  from the ABI's registers at `RET` in optimized code, one shape for
  frames in JSON, strings up to 1024 bytes; detsched: `breakpoints` says
  whether the runtime counts a hit count, and why not, and the runtime
  counts in builds with `-gcflags=all=-N -l` too;
- `0009`: detsched: `restart` to a point where its goroutine yields
  stays at the point (`runtime.detDebug.yields`), so that its position
  is the point's, and names the goroutine's frame;
- `0010`: detsched: `detsched.node` is the node's name and
  `detsched.start` which start of it; generic: a channel prints in one
  line (`chan int 3/100 (closed) at ADDR`) and `*ch` is its `hchan`, and
  `on NAME print EXPR` is labelled `EXPR`.
- `0011`: detsched: `restart NOTE` runs to the checkpoint with that note
  and other arguments are refused, and `rewind` also stops at a
  `restart` to a point; `--json` prints no prompt, also at the end of an
  `--init` file without `exit`; generic: a breakpoint names each
  function it has addresses in with its type arguments and marks a
  generic method's wrapper (`funcs` in the API and JSON); `checkpoint`
  and `checkpoints` print typed JSON records with each checkpoint's
  `id`, `note` and `pos`.
- `0012`: generic: the program dlv starts has exactly dlv's environment:
  `CGO_CFLAGS=-O0 -g` goes only to the builds of `dlv debug` and `dlv
  test`, os/exec adds no `PWD` on Linux, and on macOS debugserver
  launches the program at the protocol's `A` packet with the environment
  in `-e` options, rather than from its command line, where it adds its
  own environment (`__CF_USER_TEXT_ENCODING`); detsched: on macOS a
  detsched program's `GODEBUG` gets no `asyncpreemptoff=1`.

To build dlv for this tree, `delve.bash bin/dlv`. To change the patches,
copy Delve's module (`go mod download -json
github.com/go-delve/delve@v1.27.2` prints its directory) into a git
repository, commit each patch in order (`git apply` takes the patch, `git
commit -F` the message before its `diff --git` line), edit, and write each
commit C back as `git log -1 --format=%B C; echo; git diff C~1 C`.
`gate.bash` then tests the dlv in `bin/`.

## Publish and release

The public repository, https://github.com/ryanworl/go-detsched, holds no
development history. Each publish adds one commit there whose tree is the
tree of the development repository's main branch, and whose only parent is
the previous public commit:

```
misc/release/publish.bash -n         # make the commit, print what would be pushed
misc/release/publish.bash            # publish the tree
misc/release/publish.bash -tag       # publish it as a release
```

To release, push a commit to main that sets VERSION to the release (and
the version in the install examples of `.github/README.md` and
`misc/detsched/README.md`), run the gate, publish with
`publish.bash -m VERSION` and wait for CI to pass on GitHub, then run
`publish.bash -tag`. When the public main already has the
tree it adds no commit; either way it tags the public commit with the
version, which starts the release workflow, and with the SDK's
`misc/detsched/v0.N.0`, and tags the development commit with the version.
Then push a commit that moves VERSION to N+1.

The release workflow (`.github/workflows/release.yml`) checks that the tag
is VERSION, builds the three archives on Linux, runs `smoke.bash` on
linux/amd64, linux/arm64, and darwin/arm64 (macOS 15 and 26), and
publishes the archives and `SHA256SUMS` as the tag's GitHub release. CI
(`.github/workflows/ci.yml`) runs `build.bash` and `gate.bash` for every
push and pull request on Linux (amd64, arm64) and macOS 15 and 26 (arm64).

`publish.bash -root` starts the public history over: its commit has no
parent and replaces the public main branch.

## Take a new Go release

Branch `upstream` of the development repository holds Go's releases and
nothing else: one commit per release, whose tree is the Go repository's
tree at the release tag (the first, `Import Go go1.27.4`, is where main
started). To move to a new Go point release:

```
misc/release/upstream.bash -n go1.27.5   # report the conflicts, change nothing
misc/release/upstream.bash go1.27.5
misc/release/build.bash && misc/release/gate.bash
git push origin upstream main
```

`upstream.bash` fetches the tag from https://go.googlesource.com/go,
commits its tree on `upstream`, and merges that into the current branch.
It resolves the files the fork owns: VERSION becomes the new Go version
with the same release number, Go's files under `.github` stay replaced by
the fork's, and Go's SECURITY.md and CONTRIBUTING.md stay removed. When anything else conflicts, it lists the files and
leaves the merge for you to finish with `git commit`. Run the gate on
both Linux and darwin/arm64 before pushing, and release as above.
