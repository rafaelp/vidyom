package main

import "golang.org/x/sys/unix"

func keyctlSyscall() uintptr { return unix.SYS_KEYCTL }
