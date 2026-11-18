// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Stackread's schedule depends on the text of its own stacks, which a
// build without optimizations changes, and it always fails.
package main

import (
	"fmt"
	"os"
	"runtime"
	"sync"
)

func stackLen() int {
	buf := make([]byte, 1<<16)
	return runtime.Stack(buf, true)
}

func main() {
	var mu sync.Mutex
	var wg sync.WaitGroup
	n := 0
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < stackLen()%13+i; j++ {
				mu.Lock()
				n++
				mu.Unlock()
				runtime.Gosched()
			}
		}()
	}
	wg.Wait()
	fmt.Println("stackread: n =", n)
	os.Exit(1)
}
