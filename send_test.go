package main

import "testing"

func TestDiscoverURL(t *testing.T) {
	never := func(string) bool { return false }
	always := func(string) bool { return true }
	noGateway := func() string { return "" }
	gateway := func() string { return "192.168.122.1" }

	cases := []struct {
		name    string
		dial    func(string) bool
		resolve func(string) bool
		gateway func() string
		want    string
	}{
		{"host, daemon on loopback", always, always, gateway, "http://127.0.0.1:4649"},
		{"Lima guest", never, always, gateway, "http://host.lima.internal:4649"},
		{"libvirt guest", never, never, gateway, "http://192.168.122.1:4649"},
		{"nothing found", never, never, noGateway, "http://host.lima.internal:4649"},
	}
	for _, c := range cases {
		if got := discoverURL(c.dial, c.resolve, c.gateway); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestDaemonURLPrefersFlagThenEnvironment(t *testing.T) {
	t.Setenv("OSHIRASE_URL", "http://from-env:1")
	if got := daemonURL("http://from-flag:2"); got != "http://from-flag:2" {
		t.Errorf("flag: got %s", got)
	}
	if got := daemonURL(""); got != "http://from-env:1" {
		t.Errorf("environment: got %s", got)
	}
}
