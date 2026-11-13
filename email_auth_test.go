package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// --- The interactive grants, against fake authorization servers

// followRedirect plays the browser and the person: it reads the
// authorization URL, checks it, and sends the browser back with a code.
func followRedirect(t *testing.T, issuer string, check func(url.Values)) func(string) {
	return func(authURL string) {
		parsed, err := url.Parse(authURL)
		if err != nil {
			t.Fatal(err)
		}
		query := parsed.Query()
		check(query)
		if query.Get("code_challenge_method") != "S256" || query.Get("response_type") != "code" {
			t.Errorf("authorization URL = %s", authURL)
		}
		redirect, _ := url.Parse(query.Get("redirect_uri"))
		if redirect.Hostname() != "127.0.0.1" {
			t.Errorf("redirect_uri = %s", redirect)
		}
		back := url.Values{"code": {"code-" + query.Get("code_challenge")}, "state": {query.Get("state")}}
		if issuer != "" {
			back.Set("iss", issuer)
		}
		go func() {
			// A stray request without the state is ignored.
			if response, err := http.Get(redirect.String() + "?code=forged&state=wrong"); err == nil {
				response.Body.Close()
			}
			response, err := http.Get(redirect.String() + "?" + back.Encode())
			if err != nil {
				t.Errorf("redirect: %v", err)
				return
			}
			response.Body.Close()
		}()
	}
}

// pkceTokenEndpoint accepts a code only with the verifier for the
// challenge that code was issued against.
func pkceTokenEndpoint(t *testing.T, secret, resource, rotateTo string) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		form := r.PostForm
		sum := sha256.Sum256([]byte(form.Get("code_verifier")))
		challenge := base64.RawURLEncoding.EncodeToString(sum[:])
		if form.Get("grant_type") != "authorization_code" || form.Get("code") != "code-"+challenge {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":"invalid_grant","error_description":"PKCE mismatch"}`)
			return
		}
		if form.Get("client_secret") != secret || form.Get("resource") != resource ||
			!strings.HasPrefix(form.Get("redirect_uri"), "http://127.0.0.1:") {
			t.Errorf("token request = %v", form)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "granted-access", "expires_in": 3600,
			"refresh_token": rotateTo, "scope": gmailSendScope,
		})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestAuthGoogle(t *testing.T) {
	tokenServer := pkceTokenEndpoint(t, "the-secret", "", "granted-refresh")
	present := followRedirect(t, "", func(query url.Values) {
		if query.Get("client_id") != "the-client" || query.Get("scope") != gmailSendScope ||
			query.Get("access_type") != "offline" || query.Get("prompt") != "consent" {
			t.Errorf("authorization parameters = %v", query)
		}
	})
	client := googleClient{ID: "the-client", Secret: "the-secret", TokenEndpoint: tokenServer.URL}
	config, err := authGoogle(context.Background(), client, "https://accounts.example/auth", present, nil)
	if err != nil {
		t.Fatal(err)
	}
	if config.RefreshToken != "granted-refresh" || config.Provider != providerGoogle || config.ClientSecret != "the-secret" {
		t.Errorf("config = %+v", config)
	}
}

func TestAuthGoogleDenied(t *testing.T) {
	present := func(authURL string) {
		parsed, _ := url.Parse(authURL)
		query := parsed.Query()
		go http.Get(query.Get("redirect_uri") + "?error=access_denied&state=" + url.QueryEscape(query.Get("state")))
	}
	_, err := authGoogle(context.Background(), googleClient{ID: "c", Secret: "s", TokenEndpoint: "http://127.0.0.1:1"}, "https://accounts.example/auth", present, nil)
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("err = %v", err)
	}
}

func TestReadGoogleCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.json")
	os.WriteFile(path, []byte(`{"installed":{"client_id":"id.apps.googleusercontent.com","client_secret":"GOCSPX-x","token_uri":"https://oauth2.googleapis.com/token"}}`), 0o600)
	client, err := readGoogleCredentials(path)
	if err != nil || client.ID != "id.apps.googleusercontent.com" || client.Secret != "GOCSPX-x" {
		t.Errorf("client = %+v, err = %v", client, err)
	}
	os.WriteFile(path, []byte(`{"web":{"client_id":"x"}}`), 0o600)
	if _, err := readGoogleCredentials(path); err == nil || !strings.Contains(err.Error(), "Desktop app") {
		t.Errorf("a web client was accepted: %v", err)
	}
}

func TestAuthFastmail(t *testing.T) {
	tokenServer := pkceTokenEndpoint(t, "", "https://jmap.example/session", "granted-refresh")
	var issuer string
	var registered map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			json.NewEncoder(w).Encode(map[string]any{
				"issuer":                           issuer,
				"authorization_endpoint":           issuer + "/oauth/authorize",
				"token_endpoint":                   tokenServer.URL,
				"registration_endpoint":            issuer + "/oauth/register",
				"scopes_supported":                 []string{"urn:ietf:params:oauth:scope:mail", "offline_access"},
				"code_challenge_methods_supported": []string{"S256"},
				"authorization_response_iss_parameter_supported": true,
			})
		case "/oauth/register":
			json.NewDecoder(r.Body).Decode(&registered)
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"client_id":"dyn-123","token_endpoint_auth_method":"none"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	issuer = server.URL

	present := followRedirect(t, issuer, func(query url.Values) {
		if query.Get("client_id") != "dyn-123" || query.Get("scope") != "urn:ietf:params:oauth:scope:mail offline_access" ||
			query.Get("resource") != "https://jmap.example/session" {
			t.Errorf("authorization parameters = %v", query)
		}
	})
	config, access, err := authFastmail(context.Background(), issuer, "https://jmap.example/session", present, nil)
	if err != nil {
		t.Fatal(err)
	}
	if registered["token_endpoint_auth_method"] != "none" || registered["redirect_uris"].([]any)[0] != "http://127.0.0.1/" {
		t.Errorf("registration = %v", registered)
	}
	if config.ClientID != "dyn-123" || config.RefreshToken != "granted-refresh" || config.ClientSecret != "" ||
		config.TokenEndpoint != tokenServer.URL || access.AccessToken != "granted-access" {
		t.Errorf("config = %+v", config)
	}

	// Finding the identity uses the access token from the grant, so the
	// rotating refresh token is not spent.
	jmap := newFakeJMAP(t, func() string { return "granted-access" })
	config.JMAPSession = jmap.server.URL + "/session"
	config.To = "phone@example.com"
	if err := resolveFastmailIdentity(context.Background(), filepath.Join(t.TempDir(), "email.json"), config, access, nil); err != nil {
		t.Fatal(err)
	}
	if config.From != "other@example.com" {
		t.Errorf("From = %q, want the first identity", config.From)
	}
	if err := config.validate(); err != nil {
		t.Error(err)
	}
}

func TestReadAPIToken(t *testing.T) {
	token, err := readAPIToken(strings.NewReader("  fmu1-abc  \n"))
	if err != nil || token != "fmu1-abc" {
		t.Errorf("token = %q, err = %v", token, err)
	}
	if _, err := readAPIToken(strings.NewReader("")); err == nil {
		t.Error("empty input accepted")
	}
}

// RFC 9207 §2.4: a server that says it sends iss must send it, so a
// redirect without one is refused; one that does not say so may omit it.
func TestIssuerRequiredWhenAdvertised(t *testing.T) {
	for _, require := range []bool{true, false} {
		present := followRedirect(t, "", func(url.Values) {}) // no iss
		_, err := authorizeLoopback(context.Background(), authorizeRequest{
			Endpoint:      "https://auth.example/authorize",
			Issuer:        "https://auth.example",
			RequireIssuer: require,
			Present:       present,
		})
		switch {
		case require && (err == nil || !strings.Contains(err.Error(), "names no issuer")):
			t.Errorf("required, no iss: err = %v", err)
		case !require && err != nil:
			t.Errorf("not required, no iss: err = %v", err)
		}
	}
}

// The probe in `oshirase auth fastmail` never writes the file, even when it
// has to refresh: the file still holds the previous configuration, and an
// incomplete one (no From yet) would replace it. The rotated token is kept
// in the configuration the caller saves.
func TestProbeDoesNotSave(t *testing.T) {
	tokens := &fakeTokenEndpoint{t: t, current: "new-grant", rotate: true}
	tokenServer := httptest.NewServer(tokens)
	defer tokenServer.Close()
	jmap := newFakeJMAP(t, func() string {
		tokens.mu.Lock()
		defer tokens.mu.Unlock()
		if len(tokens.access) == 0 {
			return "none yet"
		}
		return tokens.access[len(tokens.access)-1]
	})

	path := filepath.Join(t.TempDir(), "email.json")
	previous := []byte(`{"provider":"google"}` + "\n")
	os.WriteFile(path, previous, 0o600)

	config := &emailConfig{
		Provider: providerFastmail, To: "phone@example.com",
		TokenEndpoint: tokenServer.URL, ClientID: "dyn", RefreshToken: "new-grant",
		JMAPSession: jmap.server.URL + "/session",
	}
	// An access token that counts as expired, so the probe must refresh.
	access := &tokenResponse{AccessToken: "short-lived", ExpiresIn: 30}
	if err := resolveFastmailIdentity(context.Background(), path, config, access, nil); err != nil {
		t.Fatal(err)
	}
	if tokens.requests != 1 {
		t.Fatalf("%d refreshes, want 1", tokens.requests)
	}
	if now, _ := os.ReadFile(path); string(now) != string(previous) {
		t.Errorf("the probe wrote the file: %s", now)
	}
	if config.RefreshToken != "new-grant-next" {
		t.Errorf("config holds %q, not the rotated token", config.RefreshToken)
	}
}

// --- Storing the grant

func grantConfig() *emailConfig {
	return &emailConfig{
		Provider: providerGoogle, To: "me@example.com",
		TokenEndpoint: "https://token.example", ClientID: "id",
		ClientSecret: "client-SECRET", RefreshToken: secretRefresh,
	}
}

func TestStoreGrantInTheKeyring(t *testing.T) {
	freshKeyring(t)
	path := filepath.Join(t.TempDir(), "oshirase", "email.json")
	config := grantConfig()
	var report strings.Builder
	if err := storeGrant(context.Background(), path, config, secretsInKeyring, &report); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	assertNoSecrets(t, "email.json", string(data))
	item, ok := keyringItem(t, config.GrantID)
	if !ok || item.RefreshToken != secretRefresh || item.ClientSecret != "client-SECRET" {
		t.Errorf("item = %+v, %v", item, ok)
	}

	// The daemon starts from what auth stored.
	backend, err := newEmailBackend(path)
	if err != nil {
		t.Fatal(err)
	}
	if !backend.secretsReady || backend.config.RefreshToken != secretRefresh {
		t.Errorf("daemon config = %+v", backend.config)
	}

	// Running auth again replaces the grant and deletes the old item.
	first := config.GrantID
	again := grantConfig()
	again.RefreshToken = "second-SECRET"
	if err := storeGrant(context.Background(), path, again, secretsInKeyring, &report); err != nil {
		t.Fatal(err)
	}
	if again.GrantID == first {
		t.Error("the new grant reused the old id")
	}
	if _, ok := keyringItem(t, first); ok {
		t.Error("the previous grant's item is still there")
	}
	if report.Len() != 0 {
		t.Errorf("report = %q", report.String())
	}
}

func TestStoreGrantInAFile(t *testing.T) {
	freshKeyring(t)
	path := filepath.Join(t.TempDir(), "oshirase", "email.json")
	// A keyring grant first, then -store file: the old item goes.
	keyed := grantConfig()
	storeGrant(context.Background(), path, keyed, secretsInKeyring, io.Discard)

	config := grantConfig()
	if err := storeGrant(context.Background(), path, config, secretsInFile, io.Discard); err != nil {
		t.Fatal(err)
	}
	saved, _, err := loadEmailConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Secrets != secretsInFile || saved.RefreshToken != secretRefresh || saved.GrantID != "" {
		t.Errorf("saved = %+v", saved)
	}
	if _, ok := keyringItem(t, keyed.GrantID); ok {
		t.Error("moving to -store file left the keyring item behind")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode %o", info.Mode().Perm())
	}
}

// Re-running auth over a plaintext configuration moves it to the keyring.
func TestStoreGrantMovesALegacyFile(t *testing.T) {
	freshKeyring(t)
	path := writeConfig(t, *grantConfig()) // legacy: no "secrets"
	config := grantConfig()
	if err := storeGrant(context.Background(), path, config, secretsInKeyring, io.Discard); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	assertNoSecrets(t, "email.json", string(data))
}

// When the keyring refuses, auth says so and how to do without, and leaves
// the existing configuration alone.
func TestStoreGrantKeyringFailure(t *testing.T) {
	freshKeyring(t)
	path := writeConfig(t, *grantConfig())
	before, _ := os.ReadFile(path)
	keyring.MockInitWithError(errors.New("dbus: session bus is not available"))

	err := storeGrant(context.Background(), path, grantConfig(), secretsInKeyring, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "-store file") || !strings.Contains(err.Error(), "session bus") {
		t.Errorf("err = %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("a failed keyring write changed email.json")
	}
}

// failingDelete is a keyring whose deletes fail.
type failingDelete struct{ *gatedKeyring }

func (failingDelete) Delete(string, string) error { return errors.New("delete refused") }

// When the file cannot be written and the new item cannot be removed
// either, auth names the item left behind.
func TestStoreGrantReportsALeftoverItem(t *testing.T) {
	withKeyring(t, failingDelete{newGatedKeyring()}, time.Second)
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	os.WriteFile(blocker, nil, 0o600)
	config := grantConfig()
	var report strings.Builder
	err := storeGrant(context.Background(), filepath.Join(blocker, "email.json"), config, secretsInKeyring, &report)
	if err == nil {
		t.Fatal("want the file write to fail")
	}
	if !strings.Contains(report.String(), keyringUser(config.GrantID)) || !strings.Contains(report.String(), "Leftover keyring items") {
		t.Errorf("report = %q", report.String())
	}
}
