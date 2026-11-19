// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Constructs runs three compute-bound goroutines through a wide range of
// constructs the optimized build compiles differently from the build
// without optimizations (inlined calls, closures, generics, defer,
// range-over-func, method values, switches compiled to jump tables or
// searches, string and slice builtins, and library code), so that they
// preempt each other at ticks all over them, and prints a sum.
package main

import (
	"cmp"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

var sink int
var ssink string

type pt struct{ x, y int }

func (p pt) sum() int       { return p.x + p.y }
func (p *pt) inc()          { p.x++ }
func (p pt) String() string { return strconv.Itoa(p.x) }

type summer interface{ sum() int }

func leaf(a int) int    { return a*3 + 1 }
func nonleaf(a int) int { return leaf(a) + leaf(a+1) }

//go:noinline
func noinl(a int) int { return leaf(a) }

func gen[T cmp.Ordered](xs []T) T {
	m := xs[0]
	for _, x := range xs {
		m = max(m, x)
	}
	return m
}

type box[T any] struct{ v T }

func (b *box[T]) get() T            { return b.v }
func (b *box[T]) apply(f func(T) T) { b.v = f(b.v) }

func seq(n int) iter.Seq[int] {
	return func(yield func(int) bool) {
		for i := range n {
			if !yield(i) {
				return
			}
		}
	}
}

func withDefer(a int) (r int) {
	defer func() { r += a }()
	return leaf(a)
}

func deferLoop(n int) (r int) {
	for i := range n {
		defer func() { r += i }()
	}
	return 0
}

func recoverer() (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("%v", e)
		}
	}()
	panic("x")
}

func gotoLoop(n int) int {
	i, s := 0, 0
again:
	s += i
	i++
	if i < n {
		goto again
	}
	return s
}

func intSwitch(k int) int {
	switch k {
	case 0:
		return leaf(1)
	case 1:
		return leaf(2)
	case 2:
		return 7
	case 3:
		return leaf(3)
	case 4:
		return 9
	case 5:
		return leaf(5)
	case 6:
		return 11
	case 7:
		return leaf(7)
	case 8:
		return 13
	case 9:
		return leaf(9)
	}
	return 0
}

func constSwitch(k int) int {
	switch k {
	case 0:
		return 10
	case 1:
		return 20
	case 2, 3:
		return 30
	case 4:
		return 40
	case 5:
		return 50
	}
	return -1
}

func typeSwitch(v any) int {
	switch v := v.(type) {
	case int:
		return v
	case string:
		return len(v)
	case pt:
		return v.sum()
	case *pt:
		return v.x
	case error:
		return len(v.Error())
	case fmt.Stringer:
		return len(v.String())
	case []byte:
		return len(v)
	case float64:
		return int(v)
	case bool:
		return 1
	}
	return 0
}

func strSwitch(s string) int {
	switch s {
	case "a", "bb", "ccc":
		return 1
	case "dddd", "eeeee", "ffffff":
		return 2
	case "ggggggg", "hhhhhhhh", "iiiiiiiii":
		return 3
	}
	return 0
}

var once sync.Once
var mu sync.Mutex
var rw sync.RWMutex
var ai atomic.Int64
var errX = errors.New("x")

var constructs = map[string]func(i int){
	"leaf":     func(i int) { sink += leaf(i) },
	"nonleaf":  func(i int) { sink += nonleaf(i) },
	"noinline": func(i int) { sink += noinl(i) },
	"method":   func(i int) { p := pt{i, 1}; p.inc(); sink += p.sum() },
	"iface":    func(i int) { var s summer = pt{i, 2}; sink += s.sum() },
	"ifacedyn": func(i int) {
		var s summer = pt{i, 2}
		if i%2 == 0 {
			s = &pt{i, 3}
		}
		sink += s.sum()
	},
	"closure":  func(i int) { f := func(a int) int { return a + i }; sink += f(i) },
	"closure2": func(i int) { f := func(a int) int { return nonleaf(a) + i }; sink += f(i) + f(i+1) },
	"storedcl": func(i int) {
		fs := []func() int{func() int { return i }, func() int { return leaf(i) }}
		for _, f := range fs {
			sink += f()
		}
	},
	"generic":   func(i int) { sink += gen([]int{i, 3, 9, 2}) + len(gen([]string{"a", "b"})) },
	"genbox":    func(i int) { b := &box[int]{i}; b.apply(func(v int) int { return v + 1 }); sink += b.get() },
	"defer":     func(i int) { sink += withDefer(i) },
	"deferloop": func(i int) { sink += deferLoop(i % 7) },
	"recover":   func(i int) { _ = recoverer() },
	"rangefunc": func(i int) {
		for x := range seq(i % 9) {
			sink += x
		}
	},
	"rangebreak": func(i int) {
		for x := range seq(20) {
			if x > i%5 {
				break
			}
			sink += x
		}
	},
	"mapkeys": func(i int) {
		m := map[int]int{1: 1, 2: 2, i: 3}
		for _, k := range slices.Sorted(maps.Keys(m)) {
			sink += k
		}
	},
	"slicesall": func(i int) {
		for j, v := range slices.All([]int{i, 2, 3}) {
			sink += j + v
		}
	},
	"methodval": func(i int) { p := pt{i, 1}; f := p.sum; g := (&p).inc; g(); sink += f() },
	"methodexp": func(i int) { sink += pt.sum(pt{i, 1}) + (*pt).sum(&pt{i, 2}) },
	"strconcat": func(i int) { s := "a" + strconv.Itoa(i) + "b"; ssink = s + ssink[:0] },
	"strconv":   func(i int) { b := []byte(strconv.Itoa(i)); ssink = string(b); sink += len([]rune(ssink)) },
	"rangestr": func(i int) {
		for _, r := range "héllo wörld" {
			sink += int(r)
		}
	},
	"strings": func(i int) {
		sink += strings.Index("hello world", "wor") + len(strings.Split("a,b,c", ",")) + strings.Count("cheese", "e")
	},
	"builder": func(i int) {
		var b strings.Builder
		for range 5 {
			b.WriteString("ab")
			b.WriteByte('c')
		}
		ssink = b.String()
	},
	"utf8": func(i int) { sink += utf8.RuneCountInString("héllo wörld") },
	"append": func(i int) {
		s := make([]int, 0, 2)
		for j := range 9 {
			s = append(s, j)
		}
		sink += len(s)
	},
	"copy": func(i int) { a := make([]byte, 64); b := make([]byte, 32); sink += copy(a, b) },
	"sort": func(i int) { s := []int{5, i, 3, 9, 1}; slices.Sort(s); sink += s[0] },
	"sortslice": func(i int) {
		s := []int{5, i, 3, 9, 1}
		sort.Slice(s, func(a, b int) bool { return s[a] < s[b] })
		sink += s[0]
	},
	"contains": func(i int) {
		if slices.Contains([]int{1, 2, i}, 2) {
			sink++
		}
	},
	"clear":       func(i int) { m := map[int]int{1: i}; clear(m); s := []int{1, 2}; clear(s); sink += len(m) },
	"minmax":      func(i int) { sink += min(i, 3, 9) + max(i, 2) },
	"intswitch":   func(i int) { sink += intSwitch(i % 11) },
	"constswitch": func(i int) { sink += constSwitch(i % 7) },
	"typeswitch": func(i int) {
		vs := []any{i, "ab", pt{1, 2}, &pt{3, 4}, errX, 2.5, true, []byte("x")}
		sink += typeSwitch(vs[i%len(vs)])
	},
	"strswitch": func(i int) { sink += strSwitch([]string{"a", "eeeee", "iiiiiiiii", "zz"}[i%4]) },
	"goto":      func(i int) { sink += gotoLoop(i%9 + 1) },
	"labels": func(i int) {
	outer:
		for a := range 4 {
			for b := range 4 {
				if b == a {
					continue outer
				}
				if a+b > i%6 {
					break outer
				}
				sink++
			}
		}
	},
	"loopvar": func(i int) {
		var fs []func() int
		for j := 0; j < 3; j++ {
			fs = append(fs, func() int { return j })
		}
		for _, f := range fs {
			sink += f()
		}
	},
	"once":    func(i int) { once.Do(func() { sink++ }) },
	"mutex":   func(i int) { mu.Lock(); sink++; mu.Unlock() },
	"rwmutex": func(i int) { rw.RLock(); sink++; rw.RUnlock() },
	"atomic":  func(i int) { ai.Add(1); sink += int(ai.Load()) },
	"select": func(i int) {
		c := make(chan int, 1)
		select {
		case c <- i:
		default:
		}
		select {
		case v := <-c:
			sink += v
		default:
		}
	},
	"fmt": func(i int) { ssink = fmt.Sprintf("%d %s %v", i, "x", pt{1, 2}) },
	"errors": func(i int) {
		e := fmt.Errorf("w: %w", errX)
		if errors.Is(e, errX) {
			sink++
		}
	},
	"structeq": func(i int) {
		a, b := [4]string{"a", "b"}, [4]string{"a", strconv.Itoa(i)}
		if a == b {
			sink++
		}
	},
	"ifaceeq": func(i int) {
		var a, b any = pt{i, 1}, pt{i, 1}
		if a == b {
			sink++
		}
	},
	"mapiter": func(i int) {
		m := map[string]int{"a": 1, "b": 2, "c": i}
		for _, v := range m {
			sink += v
		}
	},
	"newobj":   func(i int) { p := new(pt); p.x = i; sink += p.sum() },
	"varargs":  func(i int) { sink += vsum(i, 1, 2, 3) },
	"multiret": func(i int) { a, b := two(i); sink += a + b },
}

func vsum(xs ...int) int {
	s := 0
	for _, x := range xs {
		s += x
	}
	return s
}

func two(a int) (int, int) { return leaf(a), a }

func main() {
	names := slices.Sorted(maps.Keys(constructs))
	var wg sync.WaitGroup
	for g := range 3 {
		wg.Go(func() {
			for i := range 5000 {
				constructs[names[(i*7+g*13)%len(names)]](i + g)
			}
		})
	}
	wg.Wait()
	fmt.Println(sink % 1000)
}
