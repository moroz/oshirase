package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The small part of OAuth 2.0 that the email backend needs: the
// authorization-code grant with PKCE (RFC 7636) for an installed app with a
// loopback redirect (RFC 8252), and the refresh-token grant. It is written
// here rather than taken from golang.org/x/oauth2 because that package
// covers neither half of what is particular to this use: it does not do
// dynamic client registration (Fastmail), and it does not persist a rotated
// refresh token, which Fastmail requires (reusing the old one revokes the
// grant).

// maxOAuthResponse bounds what is read from a token or registration
// endpoint; real responses are a few hundred bytes.
const maxOAuthResponse = 1 << 20

// tokenResponse is a successful token endpoint response (RFC 6749 §5.1).
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// oauthError is an error response from a token endpoint (RFC 6749 §5.2).
// Its text never carries a token: only the status, the error code and the
// server's description.
type oauthError struct {
	Status      int    `json:"-"`
	Code        string `json:"error"`
	Description string `json:"error_description"`
	// Temporary is Fastmail's extension: the grant may work again later,
	// e.g. once an administrator lifts a lock on the account.
	Temporary bool `json:"temporary"`
}

func (e *oauthError) Error() string {
	text := e.Code
	if text == "" {
		text = "HTTP " + strconv.Itoa(e.Status)
	}
	if e.Description != "" {
		text += ": " + truncate(e.Description, 200)
	}
	return text
}

// oauthClient talks to one token endpoint as one client. ClientSecret is
// empty for a public client (Fastmail's dynamically registered ones).
type oauthClient struct {
	TokenEndpoint string
	ClientID      string
	ClientSecret  string
	HTTP          *http.Client
}

func (c oauthClient) exchange(ctx context.Context, code, verifier, redirectURI string) (*tokenResponse, error) {
	return c.post(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
	})
}

func (c oauthClient) refresh(ctx context.Context, refreshToken string) (*tokenResponse, error) {
	return c.post(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
}

func (c oauthClient) post(ctx context.Context, form url.Values) (*tokenResponse, error) {
	form.Set("client_id", c.ClientID)
	if c.ClientSecret != "" {
		form.Set("client_secret", c.ClientSecret)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	response, err := httpClient(c.HTTP).Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxOAuthResponse))
	if err != nil {
		return nil, err
	}

	if response.StatusCode != http.StatusOK {
		failure := &oauthError{Status: response.StatusCode}
		_ = json.Unmarshal(body, failure)
		return nil, failure
	}
	var token tokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("token endpoint answered with something other than JSON: %w", err)
	}
	if token.AccessToken == "" {
		return nil, errors.New("token endpoint answered without an access token")
	}
	return &token, nil
}

func httpClient(client *http.Client) *http.Client {
	if client != nil {
		return client
	}
	return http.DefaultClient
}

// pkce returns a fresh code verifier and its S256 challenge. 32 random bytes
// make a 43-character verifier, the minimum RFC 7636 allows.
func pkce() (verifier, challenge string) {
	verifier = randomToken()
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func randomToken() string {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(buffer)
}

// authorization is what the loopback redirect brought back.
type authorization struct {
	Code        string
	Verifier    string
	RedirectURI string
}

// authorizeRequest describes one authorization-code request.
type authorizeRequest struct {
	Endpoint string     // the authorization endpoint
	Params   url.Values // client_id, scope and provider extras
	// Issuer, when set, must match an `iss` parameter on the redirect
	// (RFC 9207); it is how a client tells two authorization servers apart.
	Issuer string
	// Present shows the URL to the person. The real one prints it; tests
	// follow it.
	Present func(authURL string)
	Timeout time.Duration
}

// authorizeLoopback runs the installed-app flow: it listens on a random
// port of 127.0.0.1, presents the authorization URL with a PKCE challenge
// and a state, and waits for the browser to be redirected back with a code.
func authorizeLoopback(ctx context.Context, req authorizeRequest) (*authorization, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listening for the redirect: %w", err)
	}
	defer listener.Close()

	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/", port)
	verifier, challenge := pkce()
	state := randomToken()

	params := url.Values{}
	for key, values := range req.Params {
		params[key] = values
	}
	params.Set("response_type", "code")
	params.Set("redirect_uri", redirectURI)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("state", state)
	authURL := req.Endpoint + "?" + params.Encode()

	type outcome struct {
		code string
		err  error
	}
	done := make(chan outcome, 1)
	finish := func(o outcome) {
		select {
		case done <- o:
		default:
		}
	}

	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			query := r.URL.Query()
			// Anything without our state is not the redirect we are waiting
			// for (a stray request, or a page probing the port); ignore it
			// and keep waiting.
			if query.Get("state") != state {
				http.Error(w, "unexpected request", http.StatusBadRequest)
				return
			}
			if req.Issuer != "" && query.Has("iss") && query.Get("iss") != req.Issuer {
				http.Error(w, "wrong issuer", http.StatusBadRequest)
				finish(outcome{err: fmt.Errorf("redirect came from issuer %q, expected %q", query.Get("iss"), req.Issuer)})
				return
			}
			if failure := query.Get("error"); failure != "" {
				message := failure
				if description := query.Get("error_description"); description != "" {
					message += ": " + truncate(description, 200)
				}
				fmt.Fprintln(w, "oshirase: authorization was not granted. You can close this tab.")
				finish(outcome{err: fmt.Errorf("authorization not granted (%s)", message)})
				return
			}
			code := query.Get("code")
			if code == "" {
				http.Error(w, "no code in the redirect", http.StatusBadRequest)
				finish(outcome{err: errors.New("the redirect carried no authorization code")})
				return
			}
			fmt.Fprintln(w, "oshirase: authorized. You can close this tab and go back to the terminal.")
			finish(outcome{code: code})
		}),
	}
	go server.Serve(listener)
	defer server.Close()

	req.Present(authURL)

	timeout := req.Timeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case result := <-done:
		if result.err != nil {
			return nil, result.err
		}
		return &authorization{Code: result.code, Verifier: verifier, RedirectURI: redirectURI}, nil
	case <-timer.C:
		return nil, fmt.Errorf("no redirect arrived within %s", timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}
