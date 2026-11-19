// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Environ prints its environment, one variable a line, for the tests of
// the bundled Delve in the directory above: under dlv exec it must be the
// environment dlv was given, since a detsched program that does more or
// less work for another environment runs another schedule.
package main

import (
	"fmt"
	"os"
)

func main() {
	for _, kv := range os.Environ() {
		fmt.Println(kv)
	}
}
