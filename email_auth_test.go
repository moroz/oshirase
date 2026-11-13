package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
func pkceTokenEndpoint(t *testing.T, secret string, rotateTo string) *httptest.Server {
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
		if form.Get("client_secret") != secret || !strings.HasPrefix(form.Get("redirect_uri"), "http://127.0.0.1:") {
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
	tokenServer := pkceTokenEndpoint(t, "the-secret", "granted-refresh")
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
	tokenServer := pkceTokenEndpoint(t, "", "granted-refresh")
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
		if query.Get("client_id") != "dyn-123" || query.Get("scope") != "urn:ietf:params:oauth:scope:mail offline_access" {
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
