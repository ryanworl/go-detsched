// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// App uses a module replaced by a local directory, for the tests of the
// bundled Delve in the directory above.
package main

import (
	"fmt"

	"github.com/tidewater-io/strata"
)

func main() {
	s := &strata.Set{}
	for id := 1; id <= 2; id++ {
		s.Free(id)
	}
	s.Drop(3)
	fmt.Println(strata.Visit(3))
}
