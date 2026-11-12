package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// notifierPath is where install-macos.sh puts the executable inside
// Oshirase.app. OSHIRASE_NOTIFIER overrides it.
func notifierPath() (string, error) {
	if path := os.Getenv("OSHIRASE_NOTIFIER"); path != "" {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding Oshirase.app: %w; set OSHIRASE_NOTIFIER", err)
	}
	return filepath.Join(home, "Applications", "Oshirase.app", "Contents", "MacOS", "oshirase-notifier"), nil
}

// nativeBackend does not require the app to be there yet. A daemon that
// exits over it is restarted by launchd every few seconds, and the guest
// sees a refused connection instead of a 502 saying what is missing.
func nativeBackend() (Backend, error) {
	path, err := notifierPath()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		log.Printf("warning: %v", missingApp(path, err))
	}
	return appBackend{path: path}, nil
}

func missingApp(path string, err error) error {
	if os.Getenv("OSHIRASE_NOTIFIER") != "" {
		return fmt.Errorf("no notifier at %s (from OSHIRASE_NOTIFIER): %w", path, err)
	}
	return fmt.Errorf("Oshirase.app is not installed at %s: %w; run install-macos.sh", path, err)
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
		if errors.Is(err, fs.ErrNotExist) {
			return missingApp(b.path, err)
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("Oshirase.app did not finish within %s", notifyTimeout)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if detail := strings.TrimSpace(string(output)); detail != "" {
			return errors.New(detail)
		}
		return fmt.Errorf("Oshirase.app: %w", err)
	}
	return nil
}
