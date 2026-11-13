package main

import (
	"fmt"
	"os"
	"sync"
)

// router picks the backend for one notification: the daemon's own
// (-backend) unless the notification asks for another with Via. The email
// backend is opened the first time a notification asks for it, not at
// start: a daemon showing notifications on the screen does not need a
// grant, and one made after it started is picked up without a restart.
type router struct {
	primary Backend

	// openEmail and emailSetUp are openEmailBackend and a check that its
	// configuration exists; tests replace them.
	openEmail  func() (Backend, error)
	emailSetUp func() bool

	mu    sync.Mutex
	email Backend
}

func newRouter(primary Backend) *router {
	return &router{
		primary:   primary,
		openEmail: openEmailBackend,
		emailSetUp: func() bool {
			path, err := emailConfigPath()
			if err != nil {
				return false
			}
			_, err = os.Stat(path)
			return err == nil
		},
	}
}

func (r *router) pick(via string) (Backend, error) {
	if via == "" || via == r.primary.Name() {
		return r.primary, nil
	}
	if via != ViaEmail {
		return nil, fmt.Errorf("cannot deliver via %q", via)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.email == nil {
		// A failure is not kept: the next notification tries again, so
		// running oshirase auth fixes it without a restart.
		backend, err := r.openEmail()
		if err != nil {
			return nil, fmt.Errorf("email is not set up on this host (%v); run oshirase auth there", err)
		}
		r.email = backend
	}
	return r.email, nil
}

// available lists the Via values that can be delivered now.
func (r *router) available() []string {
	if r.primary.Name() == ViaEmail || r.emailSetUp() {
		return []string{ViaEmail}
	}
	return []string{}
}
