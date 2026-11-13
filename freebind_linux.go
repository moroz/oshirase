//go:build linux

package main

import "syscall"

// IPV6_FREEBIND, which package syscall does not define.
const ipv6Freebind = 78

// freebind lets a listener bind an address that is not (yet) on any
// interface, such as a libvirt bridge's before its network starts.
func freebind(network, _ string, conn syscall.RawConn) error {
	var sockErr error
	err := conn.Control(func(fd uintptr) {
		if network == "tcp6" {
			sockErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, ipv6Freebind, 1)
		} else {
			sockErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_FREEBIND, 1)
		}
	})
	if err != nil {
		return err
	}
	return sockErr
}
