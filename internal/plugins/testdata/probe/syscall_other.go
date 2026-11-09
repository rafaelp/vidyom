//go:build !linux

package main

func keyctlSyscall() uintptr { return 0 }
