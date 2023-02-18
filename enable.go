// +build sched

package sched

import _ "unsafe"

//go:linkname grunningnanos runtime.grunningnanos
func grunningnanos() int64

func supported() bool { return true }
