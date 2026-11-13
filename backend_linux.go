//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os/exec"
	"strings"
)

// The freedesktop.org notification service: what GNOME, Cinnamon, KDE, MATE,
// XFCE, dunst and mako all answer on the session bus.
const (
	notificationsName  = "org.freedesktop.Notifications"
	notificationsPath  = "/org/freedesktop/Notifications"
	notificationsIface = "org.freedesktop.Notifications"
)

// nativeBackend does not require busctl or a notification service to be
// there yet, for the same reason as on macOS: a daemon that exits is
// restarted in a loop, and the guest sees a refused connection instead of a
// 502 saying what is missing.
func nativeBackend() (Backend, error) {
	path, err := exec.LookPath("busctl")
	if err != nil {
		log.Printf("warning: busctl not found (it comes with systemd): %v", err)
		path = "busctl"
	}
	return freedesktopBackend{busctl: path}, nil
}

func namedNativeBackend(name string) (Backend, error) {
	if name == "freedesktop" {
		return nativeBackend()
	}
	return nil, errors.New("Linux has auto, freedesktop and log")
}

// freedesktopBackend calls the notification service over D-Bus through
// busctl, which ships with systemd, so the daemon needs no D-Bus library.
// When nothing on the session bus provides the service, the call fails and
// the caller gets that as a 502.
type freedesktopBackend struct {
	busctl string
}

func (freedesktopBackend) Name() string { return "freedesktop" }

func (b freedesktopBackend) Notify(ctx context.Context, n Notification) error {
	caps, err := b.capabilities(ctx)
	if err != nil {
		return err
	}
	body := notificationBody(n, caps["body-markup"])

	// Notify(app_name, replaces_id, app_icon, summary, body, actions, hints,
	// expire_timeout). -1 leaves expiry to the server; for critical urgency
	// the specification asks servers to keep the notification until the
	// user dismisses it.
	args := []string{
		"Notify", "susssasa{sv}i",
		"oshirase", "0", "", n.Title, body,
		"0",
		"1", "urgency", "y", urgencyByte(n.Urgency),
		"-1",
	}
	_, err = b.call(ctx, args...)
	return err
}

// capabilities asks the server what it supports. Only body-markup matters:
// with it, the body is parsed as markup and must be escaped.
func (b freedesktopBackend) capabilities(ctx context.Context) (map[string]bool, error) {
	output, err := b.call(ctx, "GetCapabilities")
	if err != nil {
		return nil, err
	}
	var reply struct {
		Data [][]string `json:"data"`
	}
	if err := json.Unmarshal(output, &reply); err != nil || len(reply.Data) != 1 {
		return nil, fmt.Errorf("unexpected GetCapabilities reply from the notification service: %s", strings.TrimSpace(string(output)))
	}
	caps := map[string]bool{}
	for _, c := range reply.Data[0] {
		caps[c] = true
	}
	return caps, nil
}

func (b freedesktopBackend) call(ctx context.Context, method ...string) ([]byte, error) {
	// "--" ends busctl's options, so a title or body starting with "-" is
	// passed as text. The text travels as separate arguments; nothing runs
	// through a shell.
	args := append([]string{"--user", "--json=short", "--",
		"call", notificationsName, notificationsPath, notificationsIface}, method...)
	cmd := exec.CommandContext(ctx, b.busctl, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err == nil {
		return output, nil
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
		return nil, fmt.Errorf("busctl is not installed (it comes with systemd): %w", err)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("the notification service did not answer within %s", notifyTimeout)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if detail := strings.TrimSpace(stderr.String()); detail != "" {
		return nil, fmt.Errorf("notification service: %s", detail)
	}
	return nil, fmt.Errorf("busctl: %w", err)
}

// notificationBody puts the source on a line of its own above the body,
// since the freedesktop specification has no subtitle. With markup, the
// source is set in italics and all text is escaped.
func notificationBody(n Notification, markup bool) string {
	escape := func(s string) string { return s }
	if markup {
		escape = markupEscaper.Replace
	}
	var lines []string
	if n.Source != "" {
		if markup {
			lines = append(lines, "<i>"+escape(n.Source)+"</i>")
		} else {
			lines = append(lines, n.Source)
		}
	}
	if n.Body != "" {
		lines = append(lines, escape(n.Body))
	}
	return strings.Join(lines, "\n")
}

// The specification's markup is a small subset of XML; these three are all
// that can turn text into markup.
var markupEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func urgencyByte(u Urgency) string {
	switch u {
	case UrgencyLow:
		return "0"
	case UrgencyCritical:
		return "2"
	default:
		return "1"
	}
}
