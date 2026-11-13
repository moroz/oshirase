package main

import (
	"context"
	"fmt"
	"log"
)

// Backend shows a notification with whatever the platform provides. The
// protocol does not change with the backend; only this does.
type Backend interface {
	Name() string
	Notify(ctx context.Context, n Notification) error
}

// Withdrawer is a Backend that can take down notifications it has shown.
// Backends without it answer POST /v1/withdraw with 501.
type Withdrawer interface {
	// Withdraw closes every notification shown with the tag that may still
	// be on screen, and says how many that was.
	Withdraw(ctx context.Context, tag string) (int, error)
}

// selectBackend resolves the -backend flag. "auto" is the platform's native
// backend, defined per OS in backend_<os>.go.
func selectBackend(name string) (Backend, error) {
	switch name {
	case "auto":
		return nativeBackend()
	case "log":
		return logBackend{}, nil
	case "email":
		return openEmailBackend()
	default:
		backend, err := namedNativeBackend(name)
		if err != nil {
			return nil, fmt.Errorf("unknown backend %q: %w", name, err)
		}
		return backend, nil
	}
}

// logBackend only writes the notification to the log. It is for trying the
// daemon where no notification service exists, such as inside the VM.
type logBackend struct{}

func (logBackend) Name() string { return "log" }

func (logBackend) Notify(_ context.Context, n Notification) error {
	log.Printf("notification: urgency=%s source=%q tag=%q title=%q body=%q", n.Urgency, n.Source, n.Tag, n.Title, n.Body)
	return nil
}

func (logBackend) Withdraw(_ context.Context, tag string) (int, error) {
	log.Printf("withdraw: tag=%q", tag)
	return 0, nil
}
