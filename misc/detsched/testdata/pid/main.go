// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Pid prints its process ID. It interacts with the OS, so its output
// differs between runs; TestCheck uses it as a control.
package main

import (
	"fmt"
	"os"
)

func main() { fmt.Println(os.Getpid()) }
