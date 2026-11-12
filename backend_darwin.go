package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

func nativeBackend() (Backend, error) {
	return osascriptBackend{}, nil
}

func namedNativeBackend(name string) (Backend, error) {
	if name == "osascript" {
		return osascriptBackend{}, nil
	}
	return nil, errors.New("macOS has auto, osascript and log")
}

// osascriptBackend posts through Notification Center with AppleScript's
// `display notification`. macOS files these under Script Editor, so that is
// where they are allowed or silenced in System Settings → Notifications.
type osascriptBackend struct{}

func (osascriptBackend) Name() string { return "osascript" }

// The text travels as arguments to `on run`, never spliced into the script,
// so nothing in a title or body can be read as AppleScript.
const displayNotificationScript = `
on run argv
	set theTitle to item 1 of argv
	set theBody to item 2 of argv
	set theSubtitle to item 3 of argv
	set theSound to item 4 of argv
	if theSound is "" then
		display notification theBody with title theTitle subtitle theSubtitle
	else
		display notification theBody with title theTitle subtitle theSubtitle sound name theSound
	end if
end run
`

func (osascriptBackend) Notify(ctx context.Context, n Notification) error {
	sound := ""
	if n.Urgency == UrgencyCritical {
		sound = "Glass"
	}

	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", displayNotificationScript,
		n.Title, n.Body, n.Source, sound)
	output, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		return fmt.Errorf("osascript: %w: %s", err, detail)
	}
	return nil
}
