package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// TestMain swaps go-keyring for its in-memory mock before any test runs, so
// no test can reach the real keyring of the machine running them, even one
// that forgets to set up the mock itself.
func TestMain(m *testing.M) {
	keyring.MockInit()
	os.Exit(m.Run())
}

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
	if err := keyringStore.set("grant-a", emailSecrets{RefreshToken: secretRefresh}); err != nil {
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

	keyringStore.set("g1", emailSecrets{RefreshToken: secretRefresh, ClientSecret: "client-SECRET"})
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

// `oshirase auth` makes grant B while the daemon refreshes grant A: B's
// item and file stay as auth left them, A's item (which auth deleted) is
// not recreated, and the daemon goes on with B.
func TestStaleGrantCannotClobberANewOne(t *testing.T) {
	backend, tokens, jmap := keyringFastmailSetup(t)
	newer := *backend.config
	newer.GrantID = "grant-b"
	newer.setSecrets(emailSecrets{})
	tokens.hold = func() {
		tokens.hold = nil
		// What auth does: item first, then the file, then the old item.
		keyringStore.set("grant-b", emailSecrets{RefreshToken: "fresh-grant-SECRET"})
		if _, err := saveEmailConfig(backend.path, &newer); err != nil {
			t.Error(err)
		}
		keyringStore.remove("grant-a")
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
	notifyHandler(backend)(recorder, request)
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
	_, err := keyringStore.get("g1")
	if err == nil || time.Since(started) > 2*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(started))
	}
	if message := keyringFailure(err, config).Error(); !strings.Contains(message, "unlock prompt") || !strings.Contains(message, "-store file") {
		t.Errorf("message = %q", message)
	}

	// While that call still waits, the next one is refused at once.
	_, err = keyringStore.get("g1")
	if !errors.Is(err, errKeyringBusy) {
		t.Errorf("second call: err = %v", err)
	}

	// An unlock completes the pending call, and calls work again.
	hang.unblock()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err = keyringStore.get("g1"); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Errorf("after the unlock: %v", err)
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
