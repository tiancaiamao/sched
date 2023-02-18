// +build !sched

package sched

func grunningnanos() int64 { return 0 }

func supported() bool { return false }
