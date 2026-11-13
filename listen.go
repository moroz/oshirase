package main

import (
	"context"
	"errors"
	"log"
	"net"
	"strings"
)

// listenAddresses splits a comma-separated -listen value, dropping blanks.
func listenAddresses(value string) []string {
	var addresses []string
	for _, address := range strings.Split(value, ",") {
		if address = strings.TrimSpace(address); address != "" {
			addresses = append(addresses, address)
		}
	}
	return addresses
}

// listenAll binds every address before any is served, so a daemon that
// cannot have them all fails at once rather than running half-reachable.
// Where the platform allows (Linux), an address is bound even before it
// exists, so a VM bridge that comes up after the daemon, or goes down and up
// again, does not take loopback down with it.
func listenAll(addresses []string) ([]net.Listener, error) {
	if len(addresses) == 0 {
		return nil, errors.New("no address to listen on")
	}
	config := net.ListenConfig{Control: freebind}
	var listeners []net.Listener
	for _, address := range addresses {
		listener, err := config.Listen(context.Background(), "tcp", address)
		if err != nil {
			for _, open := range listeners {
				open.Close()
			}
			return nil, err
		}
		listeners = append(listeners, neighboursOnly(listener))
	}
	return listeners, nil
}

// neighboursOnly keeps a listener on a specific non-loopback address (a VM
// bridge's) to the hosts on that address's own network. Linux delivers a
// packet for any of its addresses on any interface, and the bridge firewall
// rules libvirt installs filter forwarded traffic, not traffic to the host:
// without this, a machine on the LAN that routes the bridge's subnet through
// this host could send notifications, and, with the email backend, mail in
// the owner's name. A forged source address cannot get past it either: the
// handshake's reply goes to the real owner of that address, on the bridge.
//
// Loopback and wildcard listeners are left alone: loopback is reachable from
// this host only, and a wildcard is an explicit choice to be reachable from
// everywhere.
func neighboursOnly(listener net.Listener) net.Listener {
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || addr.IP.IsLoopback() || addr.IP.IsUnspecified() {
		return listener
	}
	return &subnetListener{Listener: listener, local: addr.IP}
}

type subnetListener struct {
	net.Listener
	local net.IP
}

func (l *subnetListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		remote, ok := conn.RemoteAddr().(*net.TCPAddr)
		if ok && onSameNetwork(l.local, remote.IP) {
			return conn, nil
		}
		log.Printf("refused a connection to %s from %s, outside its network", l.local, conn.RemoteAddr())
		conn.Close()
	}
}

// onSameNetwork says whether remote is loopback or on the network of an
// interface that carries local. The interfaces are looked up on every
// connection, since a bridge comes and goes.
func onSameNetwork(local, remote net.IP) bool {
	if remote.IsLoopback() {
		return true
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, iface := range interfaces {
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			network, ok := address.(*net.IPNet)
			if ok && network.IP.Equal(local) && network.Contains(remote) {
				return true
			}
		}
	}
	return false
}
