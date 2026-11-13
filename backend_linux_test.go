//go:build linux

package main

import "testing"

func TestNotificationBody(t *testing.T) {
	tests := []struct {
		name   string
		n      Notification
		markup bool
		want   string
	}{
		{"body only", Notification{Body: "a & b"}, false, "a & b"},
		{"source and body, plain", Notification{Source: "ssh", Body: "push <x>"}, false, "ssh\npush <x>"},
		{"source and body, markup", Notification{Source: "a<b", Body: "push <x> & go"}, true, "<i>a&lt;b</i>\npush &lt;x&gt; &amp; go"},
		{"source only", Notification{Source: "ssh"}, true, "<i>ssh</i>"},
		{"neither", Notification{}, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := notificationBody(tt.n, tt.markup); got != tt.want {
				t.Errorf("notificationBody() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUrgencyByte(t *testing.T) {
	for u, want := range map[Urgency]string{UrgencyLow: "0", UrgencyNormal: "1", UrgencyCritical: "2", "": "1"} {
		if got := urgencyByte(u); got != want {
			t.Errorf("urgencyByte(%q) = %q, want %q", u, got, want)
		}
	}
}
