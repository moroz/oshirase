package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// notifyOnly is a backend without Withdraw.
type notifyOnly struct{}

func (notifyOnly) Name() string                               { return "notify-only" }
func (notifyOnly) Notify(context.Context, Notification) error { return nil }

func withdrawRequest(t *testing.T, backend Backend, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/withdraw", strings.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	recorder := httptest.NewRecorder()
	withdrawHandler(backend)(recorder, request)
	return recorder
}

func TestWithdrawHandler(t *testing.T) {
	tests := []struct {
		name        string
		backend     Backend
		contentType string
		body        string
		status      int
		contains    string
	}{
		{"withdrawn", logBackend{}, "application/json", `{"tag":"ssh-touch"}`, 200, `"withdrawn":0`},
		{"backend cannot", notifyOnly{}, "application/json", `{"tag":"ssh-touch"}`, 501, "cannot withdraw"},
		{"no tag", logBackend{}, "application/json", `{}`, 400, "tag is required"},
		{"tag too long", logBackend{}, "application/json", `{"tag":"` + strings.Repeat("x", 65) + `"}`, 400, "limit is 64"},
		{"not JSON", logBackend{}, "text/plain", `{"tag":"x"}`, 415, "application/json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withdrawRequest(t, tt.backend, tt.contentType, tt.body)
			if got.Code != tt.status || !strings.Contains(got.Body.String(), tt.contains) {
				t.Errorf("got %d %s, want %d containing %q", got.Code, got.Body, tt.status, tt.contains)
			}
		})
	}
}

func TestListenAddresses(t *testing.T) {
	got := strings.Join(listenAddresses(" 127.0.0.1:4649, ,192.168.122.1:4649,"), "|")
	if want := "127.0.0.1:4649|192.168.122.1:4649"; got != want {
		t.Errorf("listenAddresses() = %q, want %q", got, want)
	}
}

func TestWithdrawTooLarge(t *testing.T) {
	body := `{"tag":"` + strings.Repeat("x", maxRequestSize) + `"}`
	if got := withdrawRequest(t, logBackend{}, "application/json", body); got.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("got %d %s, want 413", got.Code, got.Body)
	}
}
