// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Subprocess starts a subprocess, which makes its schedule depend on how
// fast the OS runs it.
package main

import (
	"fmt"
	"os/exec"
)

func main() {
	fmt.Println("true:", exec.Command("true").Run())
}
