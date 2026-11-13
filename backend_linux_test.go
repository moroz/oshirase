//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBusctl writes a busctl stand-in that logs its method and arguments to
// calls.log and answers like the notification service: markup supported,
// ids 7, 8, 9... for Notify, and the server's bus name from the file owner
// (":1.1" until a test writes another, as if the server restarted).
func fakeBusctl(t *testing.T) (path, calls, owner string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls.log")
	counter := filepath.Join(dir, "id")
	owner = filepath.Join(dir, "owner")
	if err := os.WriteFile(owner, []byte(":1.1"), 0o644); err != nil {
		t.Fatal(err)
	}
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
  GetNameOwner) echo "{\"type\":\"s\",\"data\":[\"$(cat ` + owner + `)\"]}" ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, calls, owner
}

func TestFreedesktopWithdraw(t *testing.T) {
	busctl, calls, _ := fakeBusctl(t)
	b := newFreedesktopBackend(busctl)
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

	// The tagged notifications got ids 7 and 9; the untagged one, 8.
	if got, want := closedIDs(t, calls), "CloseNotification u 7|CloseNotification u 9"; got != want {
		t.Errorf("closed %q, want %q", got, want)
	}
}

func closedIDs(t *testing.T, calls string) string {
	t.Helper()
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
	return strings.Join(closed, "|")
}

// After the notification server restarts, its ids mean other notifications;
// the old ones must not be closed.
func TestFreedesktopWithdrawAfterServerRestart(t *testing.T) {
	busctl, calls, owner := fakeBusctl(t)
	b := newFreedesktopBackend(busctl)
	ctx := context.Background()
	if err := b.Notify(ctx, Notification{Title: "before", Tag: "ssh-touch", Urgency: UrgencyNormal}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(owner, []byte(":1.99"), 0o644); err != nil {
		t.Fatal(err)
	}
	count, err := b.Withdraw(ctx, "ssh-touch")
	if err != nil || count != 0 {
		t.Errorf("Withdraw() = %d, %v; want 0, nil", count, err)
	}
	if got := closedIDs(t, calls); got != "" {
		t.Errorf("closed %q after a server restart", got)
	}
}

// A withdraw that lands while Notify is still under way closes the
// notification as soon as its id is known.
func TestFreedesktopWithdrawDuringNotify(t *testing.T) {
	busctl, calls, _ := fakeBusctl(t)
	b := newFreedesktopBackend(busctl)
	ctx := context.Background()
	b.withdrawn["ssh-touch"] = time.Now().Add(time.Hour) // after any Notify starting now
	if err := b.Notify(ctx, Notification{Title: "late", Tag: "ssh-touch", Urgency: UrgencyNormal}); err != nil {
		t.Fatal(err)
	}
	if got := closedIDs(t, calls); got != "CloseNotification u 7" {
		t.Errorf("closed %q, want the notification withdrawn while it was shown", got)
	}
	if len(b.shown["ssh-touch"]) != 0 {
		t.Errorf("a notification closed at once is still remembered: %v", b.shown)
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
