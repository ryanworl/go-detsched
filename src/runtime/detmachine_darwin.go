// Copyright 2026 Ryan Worl. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// detCPUFamily returns sysctl hw.cpufamily, for the machine ID.
func detCPUFamily() (uint64, bool) {
	ret, v := sysctlbynameInt32([]byte("hw.cpufamily\x00"))
	return uint64(uint32(v)), ret == 0
}
