package main

import (
	"strings"
	"testing"
)

func TestParseDefaultGateway(t *testing.T) {
	routes := `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
enp2s0	0000A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
enp2s0	00000000	017AA8C0	0003	0	0	100	00000000	0	0	0
`
	if got := parseDefaultGateway(strings.NewReader(routes)); got != "192.168.122.1" {
		t.Errorf("got %q, want 192.168.122.1", got)
	}
	header := "Iface\tDestination\tGateway\n"
	if got := parseDefaultGateway(strings.NewReader(header)); got != "" {
		t.Errorf("no routes: got %q", got)
	}
}
