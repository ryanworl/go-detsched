// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// -json writes the search's events to a file, one JSON object per line,
// for programs that run detsched, such as a CI job (see the README's
// "JSON output"). The text output stays as it is.

// jsonSchema is the version of the events' schema, which the start
// event carries. It changes when a field changes its meaning or goes
// away; new fields and events do not change it.
const jsonSchema = 1

// A jsonWriter writes events to a file.
type jsonWriter struct {
	mu     sync.Mutex
	f      *os.File
	failed bool // a write failed, which has been reported
}

func newJSONWriter(name string) (*jsonWriter, error) {
	f, err := os.Create(name)
	if err != nil {
		return nil, err
	}
	return &jsonWriter{f: f}, nil
}

// write writes an event: its name and the time, then the fields of v, a
// struct.
func (w *jsonWriter) write(event string, v any) {
	if w == nil {
		return
	}
	head := marshal(struct {
		Event string `json:"event"`
		Time  string `json:"time"`
	}{event, time.Now().UTC().Format(time.RFC3339Nano)})
	body := marshal(v)
	line := head[:len(head)-1]
	if len(body) > 2 {
		line = append(append(line, ','), body[1:]...)
	} else {
		line = append(line, '}')
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.f.Write(append(line, '\n')); err != nil && !w.failed {
		w.failed = true
		fmt.Fprintf(os.Stderr, "detsched: -json: %v\n", err)
	}
}

func (w *jsonWriter) close() {
	if w != nil {
		w.f.Close()
	}
}

// marshal returns v as JSON, with no line break at the end, and with &,
// <, and > as they are, as in a shell command line.
func marshal(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err) // the events' types always marshal
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// seconds returns d in seconds, to the millisecond.
func seconds(d time.Duration) float64 {
	return float64(d.Round(time.Millisecond)) / float64(time.Second)
}

// jsonStart writes the start event.
func (s *search) jsonStart(run string, n uint64, d time.Duration) {
	if s.json == nil {
		return
	}
	sum := ""
	if f, err := os.Open(s.bin); err == nil {
		h := sha256.New()
		if _, err := io.Copy(h, f); err == nil {
			sum = hex.EncodeToString(h.Sum(nil))
		}
		f.Close()
	}
	mode := "search"
	if s.seeds {
		mode = "seeds"
	}
	s.json.write("start", struct {
		Schema       int      `json:"schema"`
		Detsched     string   `json:"detsched"`
		Mode         string   `json:"mode"`
		Package      string   `json:"package"`
		Binary       string   `json:"binary"`
		BinarySHA256 string   `json:"binary_sha256"`
		Cwd          string   `json:"cwd"`
		Args         []string `json:"args"`
		Run          string   `json:"run,omitempty"`
		P            int      `json:"p"`
		SeedBase     uint64   `json:"seed_base"`
		N            uint64   `json:"n,omitempty"`
		Duration     float64  `json:"duration_s,omitempty"`
		Timeout      float64  `json:"timeout_s"`
		Out          string   `json:"out"`
	}{jsonSchema, runtime.Version(), mode, s.pkg, s.bin, sum, s.dir, append([]string{}, s.args...), run, s.workers, s.seedBase, n, seconds(d), seconds(s.timeout), s.out})
}

// jsonRun writes the event of a recorded run, which failed in group g
// with its log kept in log, or passed (g 0). s.mu must be held.
func (s *search) jsonRun(r *result, g int, log string) {
	if s.json == nil {
		return
	}
	e := struct {
		Seed      uint64  `json:"seed"`
		Tape      string  `json:"tape,omitempty"`
		Yield     string  `json:"yield,omitempty"`
		Arm       string  `json:"arm"`
		Forked    bool    `json:"forked"`
		Result    string  `json:"result"`
		ExitCode  int     `json:"exit_code"`
		TimedOut  bool    `json:"timed_out,omitempty"`
		Trace     string  `json:"trace,omitempty"`
		Switches  uint64  `json:"switches,omitempty"`
		Syncs     uint64  `json:"syncs,omitempty"`
		Machine   string  `json:"machine,omitempty"`
		Elapsed   float64 `json:"elapsed_s"`
		Signature string  `json:"signature,omitempty"`
		Branch    int     `json:"branch,omitempty"`
		Group     int     `json:"group,omitempty"`
		Log       string  `json:"log,omitempty"`
	}{Seed: r.seed, Tape: r.tape, Yield: r.yield, Arm: armNames[r.arm], Forked: r.tape != "", ExitCode: r.exitCode, TimedOut: r.timedOut, Elapsed: seconds(r.wall)}
	t := r.trace()
	if m := traceHash.FindStringSubmatch(t); m != nil {
		e.Trace = m[1]
		e.Switches = atou(switchesField.FindStringSubmatch(t + " ")[1])
		e.Syncs = atou(syncsField.FindStringSubmatch(t)[1])
	}
	if m := machineLine.FindSubmatch(r.output); m != nil {
		e.Machine = string(m[1])
	}
	switch {
	case r.outOfMem:
		e.Result = "stopped"
	case r.failed():
		e.Result = "fail"
		e.Signature = r.signature()
		e.Branch, _ = r.stopped()
		e.Group, e.Log = g, log
	default:
		e.Result = "pass"
	}
	s.json.write("run", e)
}

// jsonEnd writes the fallback and group events, and the end event with
// the exit status, code, and err, the error that ended the search, if
// any.
func (s *search) jsonEnd(code int, err error) {
	if s.json == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	docs := docsPath()
	for _, f := range s.fallbacks() {
		s.json.write("fallback", struct {
			Kind string `json:"kind"`
			What string `json:"what"`
			Cost string `json:"cost"`
			Fix  string `json:"fix"`
			Docs string `json:"docs"`
		}{f.anchor, f.what, f.cost, f.fix, docs + "#" + f.anchor})
	}
	type alsoEvent struct {
		Signature string    `json:"signature"`
		Runs      int       `json:"runs"`
		Log       string    `json:"log"`
		Replay    replayCmd `json:"replay"`
	}
	for _, g := range s.groups {
		alsos := []alsoEvent{}
		for _, k := range slices.Sorted(maps.Keys(g.also)) {
			a := g.also[k]
			alsos = append(alsos, alsoEvent{a.sig, a.runs, a.log, a.replayCmd})
		}
		msg, details := splitSignature(g.sig)
		s.json.write("group", struct {
			ID           int         `json:"id"`
			Signature    string      `json:"signature"`
			Message      string      `json:"message"`
			Details      string      `json:"details,omitempty"`
			Runs         int         `json:"runs"`
			Logs         []string    `json:"logs"`
			First        any         `json:"first"`
			Replay       replayCmd   `json:"replay"`
			ReplayLine   string      `json:"replay_line"`
			Debug        replayCmd   `json:"debug"`
			DebugLine    string      `json:"debug_line"`
			Reproduced   bool        `json:"reproduced"`
			ReplayStatus string      `json:"replay_status"`
			Also         []alsoEvent `json:"also"`
			Faults       string      `json:"faults,omitempty"`
			Left         string      `json:"left,omitempty"`
			Where        string      `json:"where,omitempty"`
		}{
			g.index, g.sig, msg, details, g.runs, g.logs,
			struct {
				Seed   uint64 `json:"seed"`
				Tape   string `json:"tape,omitempty"`
				Branch int    `json:"branch"`
				Log    string `json:"log"`
			}{g.first.seed, g.first.tape, g.first.branch, g.logs[0]},
			g.replayCmd, g.replay, s.debugCommand(g), s.debugLine(g), g.status == "reproduced", g.status, alsos, g.attrib, g.left, g.where,
		})
	}
	type firstFailure struct {
		Run   int     `json:"run"`
		After float64 `json:"after_s"`
	}
	var first *firstFailure
	if s.failed > 0 {
		first = &firstFailure{s.firstRun, seconds(s.firstFail)}
	}
	var msg string
	if err != nil {
		msg = err.Error()
	}
	s.json.write("end", struct {
		Runs         int           `json:"runs"`
		Passed       int           `json:"passed"`
		Failed       int           `json:"failed"`
		Stopped      int           `json:"stopped"`
		Signatures   int           `json:"signatures"`
		Features     int           `json:"features"`
		Elapsed      float64       `json:"elapsed_s"`
		FirstFailure *firstFailure `json:"first_failure,omitempty"`
		SlowestPass  float64       `json:"slowest_pass_s"`
		SlowestFail  float64       `json:"slowest_fail_s,omitempty"`
		ExitStatus   int           `json:"exit_status"`
		Error        string        `json:"error,omitempty"`
	}{s.runs, s.passed, s.failed, s.outOfMem, len(s.groups), s.features, seconds(time.Since(s.start)), first, seconds(s.slowPass), seconds(s.slowFail), code, strings.TrimSpace(msg)})
}
