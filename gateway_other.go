//go:build !linux

package main

// defaultGateway is found on Linux only: a macOS host or Lima guest has its
// daemon on loopback or behind host.lima.internal instead.
func defaultGateway() string { return "" }
