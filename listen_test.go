package main

import (
	"net"
	"runtime"
	"testing"
)

func TestOnSameNetwork(t *testing.T) {
	var local *net.IPNet
	addresses, _ := net.InterfaceAddrs()
	for _, address := range addresses {
		if network, ok := address.(*net.IPNet); ok && !network.IP.IsLoopback() && network.IP.To4() != nil {
			local = network
			break
		}
	}
	if local == nil {
		t.Skip("no non-loopback IPv4 address on this machine")
	}
	neighbour := make(net.IP, len(local.IP.To4()))
	copy(neighbour, local.IP.To4())
	neighbour[3] ^= 1 // another host on the same network, for any prefix up to /31

	for _, tt := range []struct {
		remote net.IP
		want   bool
	}{
		{neighbour, local.Contains(neighbour)},
		{net.ParseIP("127.0.0.1"), true},
		{net.ParseIP("203.0.113.9"), local.Contains(net.ParseIP("203.0.113.9"))},
	} {
		if got := onSameNetwork(local.IP, tt.remote); got != tt.want {
			t.Errorf("onSameNetwork(%s, %s) = %v, want %v", local.IP, tt.remote, got, tt.want)
		}
	}
	// An address no interface carries has no network to be on.
	if onSameNetwork(net.ParseIP("192.0.2.1"), net.ParseIP("192.0.2.2")) {
		t.Error("onSameNetwork accepted a neighbour of an address no interface has")
	}
}

func TestNeighboursOnlyLeavesLoopbackAlone(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if _, filtered := neighboursOnly(listener).(*subnetListener); filtered {
		t.Error("a loopback listener was filtered")
	}
}

func TestListenAllBindsAbsentAddress(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("IP_FREEBIND is Linux's")
	}
	// 192.0.2.0/24 is TEST-NET-1, on no interface here.
	listeners, err := listenAll([]string{"127.0.0.1:0", "192.0.2.1:0"})
	if err != nil {
		t.Fatalf("listenAll: %v", err)
	}
	for _, listener := range listeners {
		listener.Close()
	}
	if _, filtered := listeners[1].(*subnetListener); !filtered {
		t.Error("the non-loopback listener was not filtered")
	}
}
