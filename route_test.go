package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recordingBackend remembers what it was asked to deliver.
type recordingBackend struct {
	name string
	mu   sync.Mutex
	got  []Notification
}

func (b *recordingBackend) Name() string { return b.name }
func (b *recordingBackend) Notify(_ context.Context, n Notification) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.got = append(b.got, n)
	return nil
}

func notify(t *testing.T, routes *router, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/notify", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	notifyHandler(routes)(recorder, request)
	return recorder
}

func TestViaEmailGoesToTheEmailBackend(t *testing.T) {
	screen := &recordingBackend{name: "freedesktop"}
	email := &recordingBackend{name: "email"}
	opened := 0
	routes := newRouter(screen)
	routes.openEmail = func() (Backend, error) { opened++; return email, nil }

	for _, body := range []string{`{"title":"a"}`, `{"title":"b","via":"email"}`, `{"title":"c","via":"email"}`} {
		if got := notify(t, routes, body); got.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", body, got.Code, got.Body)
		}
	}
	if len(screen.got) != 1 || screen.got[0].Title != "a" {
		t.Errorf("screen got %v", screen.got)
	}
	if len(email.got) != 2 || email.got[0].Title != "b" {
		t.Errorf("email got %v", email.got)
	}
	if opened != 1 {
		t.Errorf("the email backend was opened %d times, want once", opened)
	}

	var response NotifyResponse
	json.Unmarshal(notify(t, routes, `{"title":"d","via":"email"}`).Body.Bytes(), &response)
	if response.Backend != "email" {
		t.Errorf("response names backend %q", response.Backend)
	}
}

func TestViaEmailWithoutAGrant(t *testing.T) {
	routes := newRouter(&recordingBackend{name: "freedesktop"})
	fail := true
	routes.openEmail = func() (Backend, error) {
		if fail {
			return nil, errors.New("open email.json: no such file or directory")
		}
		return &recordingBackend{name: "email"}, nil
	}
	got := notify(t, routes, `{"title":"x","via":"email"}`)
	if got.Code != http.StatusNotImplemented || !strings.Contains(got.Body.String(), "run oshirase auth") {
		t.Errorf("got %d %s", got.Code, got.Body)
	}
	// A grant made after the daemon started is used without a restart.
	fail = false
	if got := notify(t, routes, `{"title":"x","via":"email"}`); got.Code != http.StatusOK {
		t.Errorf("after auth: %d %s", got.Code, got.Body)
	}
}

func TestViaEmailWhenEmailIsTheDaemonsBackend(t *testing.T) {
	email := &recordingBackend{name: "email"}
	routes := newRouter(email)
	routes.openEmail = func() (Backend, error) { t.Error("opened a second email backend"); return nil, errors.New("no") }
	if got := notify(t, routes, `{"title":"x","via":"email"}`); got.Code != http.StatusOK || len(email.got) != 1 {
		t.Errorf("got %d %s", got.Code, got.Body)
	}
}

func TestViaUnknown(t *testing.T) {
	got := notify(t, newRouter(&recordingBackend{name: "freedesktop"}), `{"title":"x","via":"pigeon"}`)
	if got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "via must be") {
		t.Errorf("got %d %s", got.Code, got.Body)
	}
}

func TestHealthListsVia(t *testing.T) {
	routes := newRouter(&recordingBackend{name: "freedesktop"})
	for _, setUp := range []bool{false, true} {
		routes.emailSetUp = func() bool { return setUp }
		recorder := httptest.NewRecorder()
		healthHandler(routes)(recorder, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
		var health HealthResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &health); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(health.Via, ","); (got == "email") != setUp || health.Via == nil {
			t.Errorf("set up %v: via = %q", setUp, health.Via)
		}
	}
}

// An old daemon ignores via and shows the notification; send must not
// report that as an email sent.
func TestSendNoticesAnOldDaemon(t *testing.T) {
	if err := checkDeliveredVia("email", []byte(`{"delivered":true,"backend":"freedesktop"}`)); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Errorf("err = %v", err)
	}
	if err := checkDeliveredVia("email", []byte(`{"delivered":true,"backend":"email"}`)); err != nil {
		t.Error(err)
	}
	if err := checkDeliveredVia("", []byte(`{"delivered":true,"backend":"freedesktop"}`)); err != nil {
		t.Error(err)
	}
}
