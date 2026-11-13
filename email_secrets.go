package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/zalando/go-keyring"
)

// Where the email backend's secrets live. With "secrets": "keyring" in
// email.json, the refresh token, API token and client secret are in the
// OS keyring (the Secret Service on Linux, the login Keychain on macOS), in
// one item per grant, and email.json holds only the grant's id. With
// "file", or no "secrets" at all (configurations from before the keyring),
// they are in email.json itself.
const (
	secretsInKeyring = "keyring"
	secretsInFile    = "file"

	keyringService = "oshirase"
)

// emailSecrets is the value of a keyring item, as JSON.
type emailSecrets struct {
	RefreshToken string `json:"refresh_token,omitempty"`
	APIToken     string `json:"api_token,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
}

func (c *emailConfig) secrets() emailSecrets {
	return emailSecrets{RefreshToken: c.RefreshToken, APIToken: c.APIToken, ClientSecret: c.ClientSecret}
}

func (c *emailConfig) setSecrets(s emailSecrets) {
	c.RefreshToken, c.APIToken, c.ClientSecret = s.RefreshToken, s.APIToken, s.ClientSecret
}

func (c *emailConfig) inKeyring() bool { return c.Secrets == secretsInKeyring }

// keyringUser is the item's account name. Items are keyed by grant, not by
// configuration file, so a daemon still holding an old grant can only ever
// write that grant's item, never the one `oshirase auth` has just made.
func keyringUser(grantID string) string { return "email-grant-" + grantID }

func newGrantID() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(buffer)
}

// keyringAPI is go-keyring's interface; tests replace it to make a call
// hang the way a locked keyring does.
type keyringAPI interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type osKeyring struct{}

func (osKeyring) Get(service, user string) (string, error) { return keyring.Get(service, user) }
func (osKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}
func (osKeyring) Delete(service, user string) error { return keyring.Delete(service, user) }

var (
	errKeyringTimeout = errors.New("the keyring did not answer")
	errKeyringBusy    = errors.New("an earlier keyring request is still waiting")
)

// keyringClient bounds every keyring call in time. A locked Secret Service
// answers an unlock with a prompt on the desktop, and go-keyring waits for
// it with no timeout; when nobody is at the screen — the reason to have the
// email backend at all — that is for ever. So each call runs on its own
// goroutine, the caller gets errKeyringTimeout after timeout, and the call
// stays pending (an unlock still completes it) while later calls are
// refused with errKeyringBusy instead of piling up behind it.
type keyringClient struct {
	api     keyringAPI
	timeout time.Duration

	mu      sync.Mutex
	pending bool
}

// late, passed to do, runs once a call its caller gave up on (timeout,
// context) has finished after all; see emailBackend.lateWrite.
type late func()

// keyringStore is what the daemon and `oshirase auth` use. The timeout
// bounds one call; a delivery can make several (a read when the
// configuration is loaded, a write after a refresh), and each also ends with
// the context it is given (see emailBackend.Notify for the whole budget).
// Auth raises the timeout, since a person is there to answer a prompt, and
// its context ends on Ctrl-C.
var keyringStore = &keyringClient{api: osKeyring{}, timeout: 5 * time.Second}

// do runs call with the client's timeout, or until ctx ends, whichever is
// first. Either way a call that has not returned stays pending; when it
// finishes, after its caller has gone, then (if not nil) runs.
func (k *keyringClient) do(ctx context.Context, call func() error, then late) error {
	k.mu.Lock()
	if k.pending {
		k.mu.Unlock()
		return errKeyringBusy
	}
	k.pending = true
	k.mu.Unlock()

	done := make(chan error, 1)
	var abandoned bool // under k.mu
	go func() {
		err := call()
		k.mu.Lock()
		k.pending = false
		gone := abandoned
		k.mu.Unlock()
		done <- err
		if gone && then != nil {
			then()
		}
	}()
	timer := time.NewTimer(k.timeout)
	defer timer.Stop()
	var failure error
	select {
	case err := <-done:
		return err
	case <-timer.C:
		failure = fmt.Errorf("%w within %s", errKeyringTimeout, k.timeout)
	case <-ctx.Done():
		failure = fmt.Errorf("gave up waiting for the keyring: %w", ctx.Err())
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.pending {
		// It finished as we gave up; the goroutine saw abandoned unset.
		return <-done
	}
	abandoned = true
	return failure
}

func (k *keyringClient) get(ctx context.Context, grantID string) (emailSecrets, error) {
	var value string
	err := k.do(ctx, func() (err error) {
		value, err = k.api.Get(keyringService, keyringUser(grantID))
		return err
	}, nil)
	if err != nil {
		return emailSecrets{}, err
	}
	var secrets emailSecrets
	if err := json.Unmarshal([]byte(value), &secrets); err != nil {
		// Not the error itself: it could quote the value.
		return emailSecrets{}, fmt.Errorf("the keyring item %s is not oshirase's JSON", keyringUser(grantID))
	}
	return secrets, nil
}

func (k *keyringClient) set(ctx context.Context, grantID string, secrets emailSecrets) error {
	return k.write(ctx, grantID, secrets, nil)
}

// write is set, with then run if the write finishes after ctx or the
// timeout gave up on it.
func (k *keyringClient) write(ctx context.Context, grantID string, secrets emailSecrets, then late) error {
	value, err := json.Marshal(secrets)
	if err != nil {
		return err
	}
	return k.do(ctx, func() error {
		return k.api.Set(keyringService, keyringUser(grantID), string(value))
	}, then)
}

// remove deletes a grant's item; one already gone is not an error.
func (k *keyringClient) remove(ctx context.Context, grantID string) error {
	err := k.do(ctx, func() error {
		return k.api.Delete(keyringService, keyringUser(grantID))
	}, nil)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// keyringName is what the person knows the keyring as.
func keyringName() string {
	if runtime.GOOS == "darwin" {
		return "the login Keychain"
	}
	return "the Secret Service keyring"
}

// keyringFailure says what went wrong with the keyring and what to do: the
// reason the daemon answers 502 with.
func keyringFailure(err error, config *emailConfig) error {
	auth := "oshirase auth " + config.Provider
	item := keyringUser(config.GrantID)
	switch {
	case errors.Is(err, keyring.ErrNotFound):
		return fmt.Errorf("%s has no item %s for this configuration; run %s", keyringName(), item, auth)
	case errors.Is(err, errKeyringTimeout):
		return fmt.Errorf("%v (%s): it is probably locked and waiting at an unlock prompt; unlock it, or run %s -store file", err, keyringName(), auth)
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s: %v", keyringName(), err)
	case errors.Is(err, errKeyringBusy):
		return fmt.Errorf("%s: %v, probably at an unlock prompt; unlock it, or run %s -store file", keyringName(), err, auth)
	default:
		return fmt.Errorf("%s is locked or unavailable: %v; unlock it, or run %s -store file", keyringName(), err, auth)
	}
}
