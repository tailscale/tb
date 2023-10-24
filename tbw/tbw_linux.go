package main

import "golang.org/x/sys/unix"

func init() {
	linuxMount = unix.Mount
}
