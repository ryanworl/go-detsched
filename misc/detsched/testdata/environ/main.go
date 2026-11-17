// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Environ prints its environment, one variable a line.
package main

import (
	"fmt"
	"os"
)

func main() {
	for _, e := range os.Environ() {
		fmt.Println(e)
	}
}
