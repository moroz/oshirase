package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// TestMain swaps go-keyring for its in-memory mock before any test runs, so
// no test can reach the real keyring of the machine running them, even one
// that forgets to set up the mock itself. It also skips osKeyring's look
// for a D-Bus session bus, which the mock does not need: without that, the
// keyring tests failed on a machine with no session bus (a container, CI).
func TestMain(m *testing.M) {
	keyring.MockInit()
	keyringStore.api = mockedKeyring{}
	os.Exit(m.Run())
}

// mockedKeyring is osKeyring without the session bus check; go-keyring's
// functions reach whatever provider MockInit installed last.
type mockedKeyring struct{}

func (mockedKeyring) Get(service, user string) (string, error) { return keyring.Get(service, user) }
func (mockedKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}
func (mockedKeyring) Delete(service, user string) error { return keyring.Delete(service, user) }

// freshKeyring gives the test an empty mock keyring.
func freshKeyring(t *testing.T) {
	t.Helper()
	keyring.MockInit()
	t.Cleanup(keyring.MockInit)
}

func keyringItem(t *testing.T, grantID string) (emailSecrets, bool) {
	t.Helper()
	value, err := keyring.Get(keyringService, keyringUser(grantID))
	if errors.Is(err, keyring.ErrNotFound) {
		return emailSecrets{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var secrets emailSecrets
	if err := json.Unmarshal([]byte(value), &secrets); err != nil {
		t.Fatal(err)
	}
	return secrets, true
}

// keyringFastmailSetup is fastmailSetup with the grant in the keyring.
func keyringFastmailSetup(t *testing.T) (*emailBackend, *fakeTokenEndpoint, *fakeJMAP) {
	t.Helper()
	freshKeyring(t)
	tokens := &fakeTokenEndpoint{t: t, current: secretRefresh, rotate: true}
	tokenServer := httptest.NewServer(tokens)
	t.Cleanup(tokenServer.Close)
	jmap := newFakeJMAP(t, func() string {
		tokens.mu.Lock()
		defer tokens.mu.Unlock()
		if len(tokens.access) == 0 {
			return "none yet"
		}
		return tokens.access[len(tokens.access)-1]
	})
	tokens.resource = jmap.server.URL + "/session"
	config := emailConfig{
		Provider:      providerFastmail,
		To:            "phone@example.com",
		From:          "me@fastmail.com",
		TokenEndpoint: tokenServer.URL,
		ClientID:      "dyn-client",
		JMAPSession:   jmap.server.URL + "/session",
		Secrets:       secretsInKeyring,
		GrantID:       "grant-a",
	}
	if err := keyringStore.set(context.Background(), "grant-a", emailSecrets{RefreshToken: secretRefresh}); err != nil {
		t.Fatal(err)
	}
	return newTestBackend(t, config), tokens, jmap
}

func TestKeyringModeFileHoldsNoSecrets(t *testing.T) {
	freshKeyring(t)
	path := writeConfig(t, emailConfig{
		Provider: providerGoogle, To: "me@example.com",
		TokenEndpoint: "https://token.example", ClientID: "id",
		ClientSecret: "client-SECRET", RefreshToken: secretRefresh,
		Secrets: secretsInKeyring, GrantID: "g1",
	})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, "email.json", string(data))
	if !strings.Contains(string(data), `"grant_id": "g1"`) || !strings.Contains(string(data), `"secrets": "keyring"`) {
		t.Errorf("email.json = %s", data)
	}
}

func TestDaemonLoadsSecretsFromTheKeyring(t *testing.T) {
	freshKeyring(t)
	tokens := &fakeTokenEndpoint{t: t, secret: "client-SECRET", current: secretRefresh}
	tokenServer := httptest.NewServer(tokens)
	defer tokenServer.Close()
	gmail := &fakeGmail{t: t, tokens: tokens}
	gmailServer := httptest.NewServer(gmail)
	defer gmailServer.Close()

	keyringStore.set(context.Background(), "g1", emailSecrets{RefreshToken: secretRefresh, ClientSecret: "client-SECRET"})
	backend := newTestBackend(t, emailConfig{
		Provider: providerGoogle, To: "me@example.com",
		TokenEndpoint: tokenServer.URL, ClientID: "id",
		Secrets: secretsInKeyring, GrantID: "g1",
	})
	backend.gmailSendURL = gmailServer.URL
	if err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal}); err != nil {
		t.Fatal(err)
	}
	// The fake token endpoint checked the client secret from the item.
	if len(gmail.messages) != 1 || tokens.requests != 1 {
		t.Errorf("messages %d, refreshes %d", len(gmail.messages), tokens.requests)
	}
}

func TestRotationUpdatesTheGrantsItem(t *testing.T) {
	backend, tokens, _ := keyringFastmailSetup(t)
	before, _ := os.ReadFile(backend.path)
	if err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal}); err != nil {
		t.Fatal(err)
	}
	item, ok := keyringItem(t, "grant-a")
	if !ok || item.RefreshToken != tokens.current || item.RefreshToken == secretRefresh {
		t.Errorf("item holds %+v; the endpoint now expects %q", item, tokens.current)
	}
	after, _ := os.ReadFile(backend.path)
	if string(before) != string(after) {
		t.Errorf("rotation in keyring mode rewrote email.json:\n%s", after)
	}
}

// `oshirase auth` makes grant B while the daemon's token request for grant
// A is in flight, before the daemon checks the file (for after the check,
// see TestStaleWriteAfterTheCheckLeavesNoItem): B's item and file stay as
// auth left them, A's item (which auth deleted) is not recreated, and the
// daemon goes on with B.
func TestStaleGrantDuringTheTokenRequest(t *testing.T) {
	backend, tokens, jmap := keyringFastmailSetup(t)
	newer := *backend.config
	newer.GrantID = "grant-b"
	newer.setSecrets(emailSecrets{})
	tokens.hold = func() {
		tokens.hold = nil
		// What auth does: item first, then the file, then the old item.
		keyringStore.set(context.Background(), "grant-b", emailSecrets{RefreshToken: "fresh-grant-SECRET"})
		if _, err := saveEmailConfig(backend.path, &newer); err != nil {
			t.Error(err)
		}
		keyringStore.remove(context.Background(), "grant-a")
		tokens.rotateTo = "fresh-grant-SECRET"
	}
	if err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal}); err != nil {
		t.Fatal(err)
	}
	if len(jmap.sent) != 1 {
		t.Errorf("sent %d", len(jmap.sent))
	}
	if _, ok := keyringItem(t, "grant-a"); ok {
		t.Error("the stale daemon recreated the old grant's item")
	}
	item, _ := keyringItem(t, "grant-b")
	if backend.config.GrantID != "grant-b" || item.RefreshToken != "fresh-grant-SECRET-next" {
		t.Errorf("daemon on %s, item B holds %q", backend.config.GrantID, item.RefreshToken)
	}

}

// The gap after the check: auth replaces the grant (new item, new file,
// old item deleted) after the daemon has checked that the file is
// unchanged but before its write of the rotated token, which recreates the
// old grant's item. The daemon notices on checking the file again and
// deletes it, so no live token is left behind.
func TestStaleWriteAfterTheCheckLeavesNoItem(t *testing.T) {
	logged := captureLog(t)
	backend, tokens, _ := keyringFastmailSetup(t)
	newer := *backend.config
	newer.GrantID = "grant-b"
	newer.setSecrets(emailSecrets{})
	backend.beforeItemWrite = func() {
		backend.beforeItemWrite = nil
		keyringStore.set(context.Background(), "grant-b", emailSecrets{RefreshToken: "fresh-grant-SECRET"})
		if _, err := saveEmailConfig(backend.path, &newer); err != nil {
			t.Error(err)
		}
		keyringStore.remove(context.Background(), "grant-a")
		tokens.mu.Lock()
		tokens.current = "fresh-grant-SECRET"
		tokens.mu.Unlock()
	}
	if err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal}); err != nil {
		t.Fatal(err)
	}
	if _, ok := keyringItem(t, "grant-a"); ok {
		t.Error("the old grant's item was left behind")
	}
	if item, _ := keyringItem(t, "grant-b"); item.RefreshToken != "fresh-grant-SECRET" {
		t.Errorf("item B = %q", item.RefreshToken)
	}
	if !strings.Contains(logged.String(), "deleted email-grant-grant-a") {
		t.Errorf("log: %s", logged)
	}
	// The next notification runs on the new grant.
	backend.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if err := backend.Notify(context.Background(), Notification{Title: "y", Urgency: UrgencyNormal}); err != nil {
		t.Fatal(err)
	}
	if backend.config.GrantID != "grant-b" {
		t.Errorf("daemon on %s", backend.config.GrantID)
	}
}

// A keyring that refuses is a 502 that says what is wrong and what to do,
// and the daemon starts anyway.
func TestKeyringErrorIs502(t *testing.T) {
	logged := captureLog(t)
	freshKeyring(t)
	keyring.MockInitWithError(errors.New("org.freedesktop.DBus.Error.ServiceUnknown: The name org.freedesktop.secrets was not provided"))

	backend, err := newEmailBackend(writeConfig(t, emailConfig{
		Provider: providerGoogle, To: "me@example.com",
		TokenEndpoint: "https://token.example", ClientID: "id",
		Secrets: secretsInKeyring, GrantID: "g1",
	}))
	if err != nil {
		t.Fatalf("startup failed over the keyring: %v", err)
	}
	if !strings.Contains(logged.String(), "warning: email:") {
		t.Errorf("startup did not log the keyring failure: %s", logged)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/notify", strings.NewReader(`{"title":"x","body":"`+secretBody+`"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	notifyHandler(newRouter(backend))(recorder, request)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status %d", recorder.Code)
	}
	var response ErrorResponse
	json.Unmarshal(recorder.Body.Bytes(), &response)
	for _, want := range []string{"locked or unavailable", "ServiceUnknown", "run oshirase auth google -store file"} {
		if !strings.Contains(response.Error, want) {
			t.Errorf("error %q lacks %q", response.Error, want)
		}
	}
	assertNoSecrets(t, "log", logged.String())

	// Once the keyring answers, the same daemon delivers... or at least
	// gets past the keyring: here the item is missing, which says so.
	keyring.MockInit()
	err = backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal})
	if err == nil || !strings.Contains(err.Error(), "has no item email-grant-g1") || !strings.Contains(err.Error(), "run oshirase auth google") {
		t.Errorf("err = %v", err)
	}
}

// hangingKeyring answers nothing until released, like a Secret Service
// waiting at an unlock prompt nobody sees.
type hangingKeyring struct {
	release chan struct{}
	once    sync.Once
}

func (h *hangingKeyring) wait() { <-h.release }
func (h *hangingKeyring) Get(string, string) (string, error) {
	h.wait()
	return `{"refresh_token":"r"}`, nil
}
func (h *hangingKeyring) Set(string, string, string) error { h.wait(); return nil }
func (h *hangingKeyring) Delete(string, string) error      { h.wait(); return nil }
func (h *hangingKeyring) unblock()                         { h.once.Do(func() { close(h.release) }) }
func withKeyring(t *testing.T, api keyringAPI, timeout time.Duration) {
	saved := keyringStore
	keyringStore = &keyringClient{api: api, timeout: timeout}
	t.Cleanup(func() { keyringStore = saved })
}

func TestLockedKeyringTimesOutInsteadOfHanging(t *testing.T) {
	hang := &hangingKeyring{release: make(chan struct{})}
	defer hang.unblock()
	withKeyring(t, hang, 50*time.Millisecond)
	config := &emailConfig{Provider: providerFastmail, GrantID: "g1"}

	started := time.Now()
	_, err := keyringStore.get(context.Background(), "g1")
	if err == nil || time.Since(started) > 2*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(started))
	}
	if message := keyringFailure(err, config).Error(); !strings.Contains(message, "unlock prompt") || !strings.Contains(message, "-store file") {
		t.Errorf("message = %q", message)
	}

	// While that call still waits, the next one is refused at once.
	_, err = keyringStore.get(context.Background(), "g1")
	if !errors.Is(err, errKeyringBusy) {
		t.Errorf("second call: err = %v", err)
	}

	// An unlock completes the pending call, and calls work again.
	hang.unblock()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err = keyringStore.get(context.Background(), "g1"); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Errorf("after the unlock: %v", err)
	}
}

// Ctrl-C in `oshirase auth` ends the wait for a keyring prompt at once,
// not after the two-minute timeout.
func TestKeyringWaitEndsWithTheContext(t *testing.T) {
	hang := &hangingKeyring{release: make(chan struct{})}
	defer hang.unblock()
	withKeyring(t, hang, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)

	started := time.Now()
	err := keyringStore.set(ctx, "g1", emailSecrets{RefreshToken: "r"})
	if !errors.Is(err, context.Canceled) || time.Since(started) > 2*time.Second {
		t.Errorf("err = %v after %s", err, time.Since(started))
	}
}

// A plaintext email.json from before the keyring works as it did, without
// a keyring call.
func TestLegacyPlaintextConfigNeedsNoKeyring(t *testing.T) {
	keyring.MockInitWithError(errors.New("keyring must not be used"))
	t.Cleanup(keyring.MockInit)
	backend, _, gmail := googleSetup(t)
	if backend.config.Secrets != "" {
		t.Fatalf("setup is not a legacy config: %+v", backend.config)
	}
	if err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal}); err != nil {
		t.Fatal(err)
	}
	if len(gmail.messages) != 1 {
		t.Errorf("sent %d", len(gmail.messages))
	}
}

func TestSecretsShapeValidation(t *testing.T) {
	base := emailConfig{Provider: providerGoogle, To: "a@example.com", ClientID: "c", TokenEndpoint: "t"}
	cases := map[string]func(*emailConfig){
		"keyring without grant id": func(c *emailConfig) { c.Secrets = secretsInKeyring },
		"keyring with a secret":    func(c *emailConfig) { c.Secrets, c.GrantID, c.RefreshToken = secretsInKeyring, "g", "r" },
		"unknown store":            func(c *emailConfig) { c.Secrets = "vault" },
	}
	for name, change := range cases {
		config := base
		change(&config)
		if config.validateShape() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	good := base
	good.Secrets, good.GrantID = secretsInKeyring, "g"
	if err := good.validateShape(); err != nil {
		t.Errorf("keyring mode without secrets: %v", err)
	}
}

// gatedKeyring is an in-memory keyring whose writes can be held back, like
// one waiting at an unlock prompt, and let through later.
type gatedKeyring struct {
	mu    sync.Mutex
	items map[string]string
	gate  chan struct{} // while not nil, Set waits for it to close
}

func newGatedKeyring() *gatedKeyring { return &gatedKeyring{items: map[string]string{}} }

func (g *gatedKeyring) Get(_, user string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	value, ok := g.items[user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return value, nil
}

func (g *gatedKeyring) Set(_, user, value string) error {
	g.mu.Lock()
	gate := g.gate
	g.mu.Unlock()
	if gate != nil {
		<-gate
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.items[user] = value
	return nil
}

func (g *gatedKeyring) Delete(_, user string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.items[user]; !ok {
		return keyring.ErrNotFound
	}
	delete(g.items, user)
	return nil
}

func (g *gatedKeyring) hold() {
	g.mu.Lock()
	g.gate = make(chan struct{})
	g.mu.Unlock()
}

func (g *gatedKeyring) release() {
	g.mu.Lock()
	gate := g.gate
	g.gate = nil
	g.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

func (g *gatedKeyring) token(t *testing.T, grantID string) string {
	t.Helper()
	value, err := g.Get(keyringService, keyringUser(grantID))
	if err != nil {
		return ""
	}
	var secrets emailSecrets
	json.Unmarshal([]byte(value), &secrets)
	return secrets.RefreshToken
}

// The review's sequence: the write of rotated token T1 times out and stays
// pending; the write of T2 is refused as busy; T1's write then lands late,
// so the item holds the spent T1; and a touch of email.json reloads it.
// The daemon must still refresh with T2, and the item must end up with the
// newest token, not T1.
func TestRotatedTokenSurvivesALateWriteAndAReload(t *testing.T) {
	gated := newGatedKeyring()
	defer gated.release()
	withKeyring(t, gated, 50*time.Millisecond)
	backend, tokens, jmap := keyringFastmailSetup(t)
	clock := time.Now()
	backend.now = func() time.Time { return clock }
	notify := func(step string) {
		t.Helper()
		if err := backend.Notify(context.Background(), Notification{Title: step, Urgency: UrgencyNormal}); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	t0 := secretRefresh
	t1, t2 := t0+"-next", t0+"-next-next"

	gated.hold()
	notify("rotation 1, write of T1 times out")
	if got := gated.token(t, "grant-a"); got != t0 {
		t.Fatalf("item = %q before the late write", got)
	}

	clock = clock.Add(2 * time.Hour) // the access token expires
	notify("rotation 2, write of T2 refused as busy")
	if tokens.current != t2 {
		t.Fatalf("endpoint expects %q, want T2", tokens.current)
	}

	// The held T1 write lands. The late-write hook then writes T2 over it;
	// whether or not it has yet, a reload must not go back to T1.
	gated.release()
	now := time.Now().Add(time.Second)
	os.Chtimes(backend.path, now, now) // a touch

	clock = clock.Add(2 * time.Hour)
	notify("after the reload") // refreshes with T2, or the endpoint refuses
	if len(jmap.sent) != 3 {
		t.Errorf("sent %d", len(jmap.sent))
	}
	t3 := tokens.current
	deadline := time.Now().Add(2 * time.Second)
	for gated.token(t, "grant-a") != t3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := gated.token(t, "grant-a"); got != t3 {
		t.Errorf("item = %q, want the newest token %q (not T1 %q)", got, t3, t1)
	}
}

// Without a later notification, the late write itself is followed by the
// newest token, so a daemon restart reads that and not a spent one.
func TestLateWriteIsFollowedByTheNewest(t *testing.T) {
	gated := newGatedKeyring()
	defer gated.release()
	withKeyring(t, gated, 50*time.Millisecond)
	backend, tokens, _ := keyringFastmailSetup(t)
	clock := time.Now()
	backend.now = func() time.Time { return clock }

	gated.hold()
	backend.Notify(context.Background(), Notification{Title: "1", Urgency: UrgencyNormal})
	clock = clock.Add(2 * time.Hour)
	backend.Notify(context.Background(), Notification{Title: "2", Urgency: UrgencyNormal})
	gated.release()

	deadline := time.Now().Add(2 * time.Second)
	for gated.token(t, "grant-a") != tokens.current && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := gated.token(t, "grant-a"); got != tokens.current {
		t.Errorf("item = %q, want %q", got, tokens.current)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.unsaved != nil {
		t.Errorf("unsaved still holds %q after it was written", backend.unsaved.token)
	}
}

// Without a session bus, the keyring is not tried (godbus would run
// dbus-launch), and the error says to use -store file.
func TestNoSessionBus(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	runUser := t.TempDir()
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	err := findSessionBus("linux", runUser)
	if !errors.Is(err, errNoSessionBus) {
		t.Fatalf("err = %v", err)
	}
	message := keyringFailure(err, &emailConfig{Provider: providerGoogle}).Error()
	if !strings.Contains(message, "no D-Bus session bus, so no Secret Service") || !strings.Contains(message, "run oshirase auth google -store file") {
		t.Errorf("message = %q", message)
	}
	if findSessionBus("darwin", runUser) != nil {
		t.Error("macOS needs no session bus")
	}

	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "autolaunch:")
	if findSessionBus("linux", runUser) == nil {
		t.Error("autolaunch: counted as a bus; godbus would run dbus-launch for it")
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/somewhere")
	if err := findSessionBus("linux", runUser); err != nil {
		t.Errorf("with an address: %v", err)
	}

	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	os.WriteFile(filepath.Join(runUser, "bus"), nil, 0o600)
	if err := findSessionBus("linux", runUser); err != nil {
		t.Errorf("with /run/user/<uid>/bus: %v", err)
	}
	os.Remove(filepath.Join(runUser, "bus"))

	os.WriteFile(filepath.Join(runtimeDir, "bus"), nil, 0o600)
	if err := findSessionBus("linux", runUser); err != nil {
		t.Errorf("with $XDG_RUNTIME_DIR/bus: %v", err)
	}
	if got := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); got != "unix:path="+filepath.Join(runtimeDir, "bus") {
		t.Errorf("DBUS_SESSION_BUS_ADDRESS = %q", got)
	}
}
