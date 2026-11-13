//go:build !darwin && !linux

package main

import (
	"errors"
	"runtime"
)

func nativeBackend() (Backend, error) {
	return nil, errors.New("no native backend on " + runtime.GOOS + " yet; use -backend log")
}

func namedNativeBackend(string) (Backend, error) {
	return nil, errors.New("only auto and log exist on " + runtime.GOOS)
}
