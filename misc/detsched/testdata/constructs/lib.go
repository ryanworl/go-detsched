// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bufio"
	"bytes"
	"container/heap"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"math"
	"math/big"
	"math/rand/v2"
	"regexp"
	"strings"
	"text/template"
	"time"
	"unique"
)

type ih []int

func (h ih) Len() int           { return len(h) }
func (h ih) Less(i, j int) bool { return h[i] < h[j] }
func (h ih) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *ih) Push(x any)        { *h = append(*h, x.(int)) }
func (h *ih) Pop() any          { o := *h; x := o[len(o)-1]; *h = o[:len(o)-1]; return x }

type rec struct {
	A int             `json:"a"`
	B string          `json:"b"`
	C []float64       `json:"c"`
	D map[string]bool `json:"d"`
	E *rec            `json:"e,omitempty"`
}

var re = regexp.MustCompile(`a(b+)c|x[0-9]+y`)
var tmpl = template.Must(template.New("t").Parse(`{{range .}}{{.A}}-{{.B}};{{end}}`))
var rng = rand.New(rand.NewPCG(1, 2))
var repl = strings.NewReplacer("a", "1", "b", "2")

func idx(s []int, i int) (r int) {
	defer func() {
		if recover() != nil {
			r = -1
		}
	}()
	return s[i]
}

func init() {
	for k, f := range map[string]func(i int){
		"json": func(i int) {
			b, _ := json.Marshal(rec{A: i, B: "x", C: []float64{1.5}, D: map[string]bool{"k": true}, E: &rec{A: 2}})
			var r rec
			_ = json.Unmarshal(b, &r)
			sink += r.A
		},
		"buffer": func(i int) { var b bytes.Buffer; b.WriteString("hello"); b.WriteByte(byte(i)); sink += b.Len() },
		"bufio": func(i int) {
			s := bufio.NewScanner(strings.NewReader("a b\nc d\ne"))
			for s.Scan() {
				sink += len(s.Text())
			}
		},
		"heap":   func(i int) { h := &ih{5, 2, i % 9}; heap.Init(h); heap.Push(h, 3); sink += heap.Pop(h).(int) },
		"sha256": func(i int) { s := sha256.Sum256([]byte("hello world")); sink += int(s[0]) },
		"crc32":  func(i int) { sink += int(crc32.ChecksumIEEE([]byte("hello world"))) },
		"binary": func(i int) {
			b := binary.AppendUvarint(nil, uint64(i))
			v, _ := binary.Uvarint(b)
			sink += int(v) + int(binary.BigEndian.Uint32([]byte{1, 2, 3, 4}))
		},
		"math":   func(i int) { sink += int(math.Sqrt(float64(i)) + math.Sin(float64(i)) + math.Pow(2, 3) + math.Log(5)) },
		"bigint": func(i int) { a := big.NewInt(int64(i)); a.Exp(a, big.NewInt(20), nil); sink += a.BitLen() },
		"rand":   func(i int) { sink += rng.IntN(100) + int(rng.Uint64()%3) },
		"regexp": func(i int) {
			if re.MatchString("zzabbbcz") {
				sink++
			}
			sink += len(re.FindAllString("x12y abc", -1))
		},
		"template": func(i int) { var b bytes.Buffer; _ = tmpl.Execute(&b, []rec{{A: i, B: "q"}}); sink += b.Len() },
		"timefmt": func(i int) {
			t := time.Unix(int64(i)*1000, 0).UTC()
			ssink = t.Format(time.RFC3339)
			_, _ = time.Parse(time.RFC3339, ssink)
		},
		"replacer": func(i int) { ssink = repl.Replace("abcab") },
		"unique":   func(i int) { h := unique.Make("abc"); ssink = h.Value() },
		"panicidx": func(i int) { sink += idx([]int{1, 2}, i%4) },
		"chanrange": func(i int) {
			c := make(chan int, 4)
			for j := range 4 {
				c <- j
			}
			close(c)
			for v := range c {
				sink += v
			}
		},
		"goroutine": func(i int) { c := make(chan int); go func() { c <- leaf(i) }(); sink += <-c },
		"bigstruct": func(i int) { var a [64]int; a[i%64] = i; b := a; sink += b[3] },
		"stringsfn": func(i int) {
			sink += len(strings.Fields(" a b  c ")) + len(strings.ToUpper("abc")) + len(strings.TrimSpace("  x "))
			sink += len(strings.Repeat("ab", 3)) + strings.LastIndex("abcabc", "b")
			if strings.EqualFold("Go", "GO") && strings.HasPrefix("golang", "go") {
				sink++
			}
		},
		"sprintln": func(i int) { ssink = fmt.Sprint(i, "a", 2.5, []int{1}, map[string]int{"a": 1}) },
	} {
		constructs[k] = f
	}
}
