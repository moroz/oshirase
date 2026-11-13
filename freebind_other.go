//go:build !linux

package main

import "syscall"

// freebind is Linux's alone; elsewhere an address must exist to be bound.
func freebind(string, string, syscall.RawConn) error { return nil }
