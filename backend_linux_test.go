//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBusctl writes a busctl stand-in that logs its method and arguments to
// calls.log and answers like the notification service: markup supported,
// and ids 7, 8, 9... for Notify.
func fakeBusctl(t *testing.T) (path, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls.log")
	counter := filepath.Join(dir, "id")
	path = filepath.Join(dir, "busctl")
	script := `#!/bin/sh
# argv: --user --json=short -- call NAME PATH IFACE METHOD ARGS...
shift 7
method=$1; shift
echo "$method $*" >> ` + calls + `
case $method in
  GetCapabilities) echo '{"type":"as","data":[["body","body-markup"]]}' ;;
  Notify) id=$(cat ` + counter + ` 2>/dev/null || echo 7); echo $((id + 1)) > ` + counter + `
          echo "{\"type\":\"u\",\"data\":[$id]}" ;;
  CloseNotification) echo '{"type":"","data":[]}' ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, calls
}

func TestFreedesktopWithdraw(t *testing.T) {
	busctl, calls := fakeBusctl(t)
	b := &freedesktopBackend{busctl: busctl, shown: map[string][]uint32{}}
	ctx := context.Background()

	for _, n := range []Notification{
		{Title: "Touch key: push a", Tag: "ssh-touch", Urgency: UrgencyNormal},
		{Title: "untagged", Urgency: UrgencyNormal},
		{Title: "Touch key: fetch b", Tag: "ssh-touch", Urgency: UrgencyNormal},
	} {
		if err := b.Notify(ctx, n); err != nil {
			t.Fatalf("Notify(%q): %v", n.Title, err)
		}
	}

	count, err := b.Withdraw(ctx, "ssh-touch")
	if err != nil || count != 2 {
		t.Fatalf("Withdraw() = %d, %v; want 2, nil", count, err)
	}
	if count, _ := b.Withdraw(ctx, "ssh-touch"); count != 0 {
		t.Errorf("second Withdraw() = %d, want 0", count)
	}

	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	var closed []string
	for _, line := range strings.Split(string(log), "\n") {
		if strings.HasPrefix(line, "CloseNotification") {
			closed = append(closed, line)
		}
	}
	// The tagged notifications got ids 7 and 9; the untagged one, 8.
	if got, want := strings.Join(closed, "|"), "CloseNotification u 7|CloseNotification u 9"; got != want {
		t.Errorf("closed %q, want %q", got, want)
	}
}

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
