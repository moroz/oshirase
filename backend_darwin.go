package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// notifierPath is where install-macos.sh puts the executable inside
// Oshirase.app. OSHIRASE_NOTIFIER overrides it.
func notifierPath() string {
	if path := os.Getenv("OSHIRASE_NOTIFIER"); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Applications", "Oshirase.app", "Contents", "MacOS", "oshirase-notifier")
}

func nativeBackend() (Backend, error) {
	path := notifierPath()
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("Oshirase.app is not installed (%w); run install-macos.sh", err)
	}
	return appBackend{path: path}, nil
}

func namedNativeBackend(name string) (Backend, error) {
	if name == "macos" {
		return nativeBackend()
	}
	return nil, errors.New("macOS has auto, macos and log")
}

// appBackend posts through Oshirase.app, a small signed bundle that uses the
// UserNotifications framework. macOS lists it as "oshirase" in System
// Settings → Notifications, and the app exits non-zero when macOS has
// notifications for it turned off, so a refusal reaches the caller.
type appBackend struct {
	path string
}

func (appBackend) Name() string { return "macos" }

func (b appBackend) Notify(ctx context.Context, n Notification) error {
	// The text travels as separate arguments; nothing runs through a shell.
	cmd := exec.CommandContext(ctx, b.path, "post", n.Title, n.Body, n.Source, string(n.Urgency))
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("Oshirase.app did not finish in time: %w", ctx.Err())
		}
		if detail := strings.TrimSpace(string(output)); detail != "" {
			return errors.New(detail)
		}
		return fmt.Errorf("Oshirase.app: %w", err)
	}
	return nil
}
