package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	secretRefresh = "refresh-SECRET-1"
	secretAccess  = "access-SECRET-1"
	secretBody    = "body that must not be logged"
)

// fakeTokenEndpoint answers the refresh grant. It rotates the refresh
// token when rotate is set, and refuses with failure when that is set.
type fakeTokenEndpoint struct {
	mu       sync.Mutex
	t        *testing.T
	secret   string // expected client_secret; "" for a public client
	current  string // the refresh token it accepts
	rotate   bool
	failure  string // a JSON error body to answer with, status 400
	hold     func() // runs before answering, e.g. to make the caller give up
	rotateTo string // after this answer, accept this refresh token instead
	resource string // the resource (RFC 8707) a refresh must name; "" for none
	requests int
	access   []string // access tokens it has issued
}

func (f *fakeTokenEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("token request: %v", err)
	}
	if got := r.PostForm.Get("client_secret"); got != f.secret {
		f.t.Errorf("client_secret = %q, want %q", got, f.secret)
	}
	if got := r.PostForm.Get("resource"); got != f.resource {
		f.t.Errorf("resource = %q, want %q", got, f.resource)
	}
	if f.hold != nil {
		f.hold()
	}
	if f.failure != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, f.failure)
		return
	}
	if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("refresh_token") != f.current {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant"}`)
		return
	}
	access := secretAccess + "-" + string(rune('a'+len(f.access)))
	f.access = append(f.access, access)
	response := map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 3600}
	if f.rotate {
		response["refresh_token"] = f.current + "-next"
		f.current = f.current + "-next"
		if f.rotateTo != "" {
			// Something else (a new grant) is what is valid from now on.
			f.current, f.rotateTo = f.rotateTo, ""
		}
	}
	json.NewEncoder(w).Encode(response)
}

func writeConfig(t *testing.T, config emailConfig) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oshirase", "email.json")
	if _, err := saveEmailConfig(path, &config); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestBackend(t *testing.T, config emailConfig) *emailBackend {
	t.Helper()
	backend, err := newEmailBackend(writeConfig(t, config))
	if err != nil {
		t.Fatal(err)
	}
	return backend
}

// captureLog collects what the daemon logs for the length of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	var buffer bytes.Buffer
	log.SetOutput(&buffer)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buffer
}

func assertNoSecrets(t *testing.T, where, text string) {
	t.Helper()
	for _, secret := range []string{"SECRET", secretBody} {
		if strings.Contains(text, secret) {
			t.Errorf("%s contains %q: %s", where, secret, text)
		}
	}
}

// --- Gmail

type fakeGmail struct {
	mu       sync.Mutex
	t        *testing.T
	tokens   *fakeTokenEndpoint
	reject   int // answer this many requests with 401 first
	status   int // when not 0, fail with this status and a Google error
	messages []*mail.Message
	auth     []string
}

func (f *fakeGmail) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	if f.reject > 0 {
		f.reject--
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"code":401,"status":"UNAUTHENTICATED","message":"Invalid Credentials"}}`)
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		io.WriteString(w, `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"Request had insufficient authentication scopes."}}`)
		return
	}
	var request struct {
		Raw string `json:"raw"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		f.t.Fatalf("gmail request: %v", err)
	}
	raw, err := base64.URLEncoding.DecodeString(request.Raw)
	if err != nil {
		f.t.Fatalf("raw is not base64url: %v", err)
	}
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		f.t.Fatalf("raw is not a message: %v", err)
	}
	f.messages = append(f.messages, message)
	io.WriteString(w, `{"id":"1","threadId":"1","labelIds":["SENT"]}`)
}

func googleSetup(t *testing.T) (*emailBackend, *fakeTokenEndpoint, *fakeGmail) {
	tokens := &fakeTokenEndpoint{t: t, secret: "client-SECRET", current: secretRefresh}
	tokenServer := httptest.NewServer(tokens)
	t.Cleanup(tokenServer.Close)
	gmail := &fakeGmail{t: t, tokens: tokens}
	gmailServer := httptest.NewServer(gmail)
	t.Cleanup(gmailServer.Close)

	backend := newTestBackend(t, emailConfig{
		Provider:      providerGoogle,
		To:            "me@example.com",
		TokenEndpoint: tokenServer.URL,
		ClientID:      "client-id",
		ClientSecret:  "client-SECRET",
		RefreshToken:  secretRefresh,
	})
	backend.gmailSendURL = gmailServer.URL
	return backend, tokens, gmail
}

func TestGmailSendsTheNotification(t *testing.T) {
	backend, tokens, gmail := googleSetup(t)
	n := Notification{Title: "Touch your security key", Body: "git push origin\nto main", Urgency: UrgencyCritical, Source: "ssh"}
	if err := backend.Notify(context.Background(), n); err != nil {
		t.Fatal(err)
	}

	if len(gmail.messages) != 1 {
		t.Fatalf("gmail got %d messages", len(gmail.messages))
	}
	message := gmail.messages[0]
	subject, err := new(mime.WordDecoder).DecodeHeader(message.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "[oshirase · ssh] Touch your security key"; subject != want {
		t.Errorf("subject = %q, want %q", subject, want)
	}
	if got := message.Header.Get("To"); got != "<me@example.com>" {
		t.Errorf("To = %q", got)
	}
	if message.Header.Get("From") != "" {
		t.Errorf("From = %q; Gmail should fill it in", message.Header.Get("From"))
	}
	if message.Header.Get("Importance") != "high" || !strings.HasPrefix(message.Header.Get("X-Priority"), "1") {
		t.Errorf("critical is not marked important: %v", message.Header)
	}
	body, _ := io.ReadAll(message.Body)
	if !strings.Contains(string(body), "git push origin\r\nto main") {
		t.Errorf("body = %q", body)
	}
	if gmail.auth[0] != "Bearer "+tokens.access[0] {
		t.Errorf("Authorization = %q", gmail.auth[0])
	}
}

func TestGmailReusesTheAccessToken(t *testing.T) {
	backend, tokens, gmail := googleSetup(t)
	for range 3 {
		if err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal}); err != nil {
			t.Fatal(err)
		}
	}
	if tokens.requests != 1 {
		t.Errorf("refreshed %d times for 3 notifications, want 1", tokens.requests)
	}
	if len(gmail.messages) != 3 {
		t.Errorf("sent %d", len(gmail.messages))
	}
	if gmail.messages[0].Header.Get("X-Priority") != "" {
		t.Error("normal urgency should add no priority header")
	}

	// Past expiry, it refreshes again.
	backend.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal}); err != nil {
		t.Fatal(err)
	}
	if tokens.requests != 2 {
		t.Errorf("refreshed %d times after expiry, want 2", tokens.requests)
	}
}

func TestGmailRetriesOnceAfter401(t *testing.T) {
	backend, tokens, gmail := googleSetup(t)
	gmail.reject = 1
	if err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal}); err != nil {
		t.Fatal(err)
	}
	if tokens.requests != 2 || len(gmail.messages) != 1 {
		t.Errorf("refreshes = %d, messages = %d; want 2 and 1", tokens.requests, len(gmail.messages))
	}

	gmail.reject = 2
	err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal})
	if err == nil || !strings.Contains(err.Error(), "run oshirase auth google") {
		t.Errorf("err = %v", err)
	}
}

func TestGmailRefusalReachesTheCaller(t *testing.T) {
	backend, _, gmail := googleSetup(t)
	gmail.status = http.StatusForbidden
	err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal})
	if err == nil || !strings.Contains(err.Error(), "PERMISSION_DENIED") {
		t.Errorf("err = %v", err)
	}
}

// A revoked grant is a 502 that says what to run, and nothing secret
// reaches the log or the response.
func TestRevokedRefreshTokenIs502WithAdvice(t *testing.T) {
	logged := captureLog(t)
	backend, tokens, _ := googleSetup(t)
	tokens.failure = `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`

	handler := notifyHandler(backend)
	request := httptest.NewRequest(http.MethodPost, "/v1/notify",
		strings.NewReader(`{"title":"Touch key","body":"`+secretBody+`","source":"ssh"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler(recorder, request)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", recorder.Code)
	}
	var response ErrorResponse
	json.Unmarshal(recorder.Body.Bytes(), &response)
	if !strings.Contains(response.Error, "refresh token revoked or expired") ||
		!strings.Contains(response.Error, "run oshirase auth google") {
		t.Errorf("error = %q", response.Error)
	}
	assertNoSecrets(t, "response", recorder.Body.String())
	assertNoSecrets(t, "log", logged.String())
}

func TestSubjectCannotInjectHeaders(t *testing.T) {
	message := composeEmail(Notification{Title: "a\r\nBcc: evil@example.com", Source: "x\ny"})
	raw := buildRFC5322("", "me@example.com", message, time.Now())
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Header.Get("Bcc") != "" {
		t.Errorf("title injected a header: %q", raw)
	}
	if message.Subject != "[oshirase · x y] a Bcc: evil@example.com" {
		t.Errorf("subject = %q", message.Subject)
	}
	// With no body, the title is the body.
	if message.Body != "a\r\nBcc: evil@example.com" {
		t.Errorf("body = %q", message.Body)
	}
}

// A long non-ASCII subject is folded between encoded words, so no header
// line passes RFC 5322's 998 characters. (The first line, "Subject: " and
// a 75-character word, may pass the 78 the RFC recommends.)
func TestLongSubjectIsFolded(t *testing.T) {
	n := Notification{Title: strings.Repeat("通知", 128), Source: strings.Repeat("é", 64)}
	message := composeEmail(n)
	raw := buildRFC5322("", "me@example.com", message, time.Now())
	header, _, _ := bytes.Cut(raw, []byte("\r\n\r\n"))
	for _, line := range strings.Split(string(header), "\r\n") {
		if len(line) > 9+75 {
			t.Errorf("header line of %d characters: %.60s...", len(line), line)
		}
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	if subject != message.Subject {
		t.Errorf("subject came back as %q", subject)
	}
	if encodeSubject("[oshirase] plain") != "[oshirase] plain" {
		t.Error("a plain ASCII subject should go as it is")
	}
}

// --- Fastmail / JMAP

type fakeJMAP struct {
	mu          sync.Mutex
	t           *testing.T
	server      *httptest.Server
	token       func() string // the bearer it accepts
	submission  bool
	notCreated  bool
	sent        []map[string]any // Email/set create objects
	submissions []map[string]any
	sessions    int
}

func newFakeJMAP(t *testing.T, token func() string) *fakeJMAP {
	f := &fakeJMAP{t: t, token: token, submission: true}
	f.server = httptest.NewServer(f)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeJMAP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.token() {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/session":
		f.sessions++
		capabilities := map[string]any{jmapCore: map[string]any{}, jmapMail: map[string]any{}}
		primary := map[string]string{jmapMail: "acc1"}
		if f.submission {
			capabilities[jmapSubmission] = map[string]any{}
			primary[jmapSubmission] = "acc1"
		}
		json.NewEncoder(w).Encode(map[string]any{
			"apiUrl":          f.server.URL + "/api",
			"capabilities":    capabilities,
			"primaryAccounts": primary,
		})
	case "/api":
		var request struct {
			Using       []string            `json:"using"`
			MethodCalls [][]json.RawMessage `json:"methodCalls"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			f.t.Fatalf("api request: %v", err)
		}
		var responses []any
		for _, call := range request.MethodCalls {
			var name, id string
			var args map[string]any
			json.Unmarshal(call[0], &name)
			json.Unmarshal(call[1], &args)
			json.Unmarshal(call[2], &id)
			if args["accountId"] != "acc1" {
				f.t.Errorf("%s accountId = %v", name, args["accountId"])
			}
			var result any
			switch name {
			case "Identity/get":
				result = map[string]any{"list": []any{
					map[string]string{"id": "id-other", "email": "other@example.com"},
					map[string]string{"id": "id-me", "email": "me@fastmail.com"},
				}}
			case "Mailbox/query":
				role := args["filter"].(map[string]any)["role"]
				result = map[string]any{"ids": []string{"mb-" + role.(string)}}
			case "Email/set":
				create := args["create"].(map[string]any)["draft"].(map[string]any)
				f.sent = append(f.sent, create)
				if f.notCreated {
					result = map[string]any{"notCreated": map[string]any{"draft": map[string]string{"type": "overQuota", "description": "mailbox full"}}}
				} else {
					result = map[string]any{"created": map[string]any{"draft": map[string]string{"id": "e1"}}}
				}
			case "EmailSubmission/set":
				f.submissions = append(f.submissions, args)
				result = map[string]any{"created": map[string]any{"send": map[string]string{"id": "s1"}}}
			default:
				f.t.Errorf("unexpected method %s", name)
			}
			responses = append(responses, []any{name, result, id})
		}
		json.NewEncoder(w).Encode(map[string]any{"methodResponses": responses, "sessionState": "1"})
	default:
		http.NotFound(w, r)
	}
}

func fastmailSetup(t *testing.T) (*emailBackend, *fakeTokenEndpoint, *fakeJMAP) {
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
	backend := newTestBackend(t, emailConfig{
		Provider:      providerFastmail,
		To:            "phone@example.com",
		From:          "me@fastmail.com",
		TokenEndpoint: tokenServer.URL,
		ClientID:      "dyn-client",
		RefreshToken:  secretRefresh,
		JMAPSession:   jmap.server.URL + "/session",
	})
	return backend, tokens, jmap
}

func TestJMAPSendsAndPersistsTheRotatedRefreshToken(t *testing.T) {
	logged := captureLog(t)
	backend, tokens, jmap := fastmailSetup(t)
	n := Notification{Title: "Tests failed", Body: secretBody, Urgency: UrgencyLow, Source: "oshirase · email-backend"}
	if err := backend.Notify(context.Background(), n); err != nil {
		t.Fatal(err)
	}

	if len(jmap.sent) != 1 || len(jmap.submissions) != 1 {
		t.Fatalf("sent %d, submitted %d", len(jmap.sent), len(jmap.submissions))
	}
	email := jmap.sent[0]
	if email["subject"] != "[oshirase · oshirase · email-backend] Tests failed" {
		t.Errorf("subject = %v", email["subject"])
	}
	if from := email["from"].([]any)[0].(map[string]any)["email"]; from != "me@fastmail.com" {
		t.Errorf("from = %v", from)
	}
	if to := email["to"].([]any)[0].(map[string]any)["email"]; to != "phone@example.com" {
		t.Errorf("to = %v", to)
	}
	if email["header:Importance:asText"] != "low" {
		t.Errorf("low urgency not marked: %v", email)
	}
	if _, ok := email["mailboxIds"].(map[string]any)["mb-drafts"]; !ok {
		t.Errorf("not created in Drafts: %v", email["mailboxIds"])
	}
	submission := jmap.submissions[0]
	created := submission["create"].(map[string]any)["send"].(map[string]any)
	if created["identityId"] != "id-me" || created["emailId"] != "#draft" {
		t.Errorf("submission = %v", created)
	}
	update := submission["onSuccessUpdateEmail"].(map[string]any)["#send"].(map[string]any)
	if update["mailboxIds/mb-sent"] != true {
		t.Errorf("not moved to Sent: %v", update)
	}

	// The rotated refresh token is on disk, still private.
	saved, _, err := loadEmailConfig(backend.path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.RefreshToken != tokens.current {
		t.Errorf("saved refresh token %q, endpoint now expects %q", saved.RefreshToken, tokens.current)
	}
	info, _ := os.Stat(backend.path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}

	// The second notification uses the cached session and access token.
	if err := backend.Notify(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if jmap.sessions != 1 || tokens.requests != 1 {
		t.Errorf("sessions = %d, refreshes = %d; want 1 and 1", jmap.sessions, tokens.requests)
	}
	assertNoSecrets(t, "log", logged.String())
}

// A caller that gives up during the refresh must not cost the rotated
// refresh token: the refresh finishes and the new token is saved.
func TestRotatedTokenSurvivesTheCallerGivingUp(t *testing.T) {
	backend, tokens, _ := fastmailSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	tokens.hold = func() {
		cancel()
		time.Sleep(50 * time.Millisecond) // let the cancellation land
	}
	if err := backend.Notify(ctx, Notification{Title: "x", Urgency: UrgencyNormal}); err == nil {
		t.Fatal("want the send to fail on the cancelled context")
	}
	saved, _, err := loadEmailConfig(backend.path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.RefreshToken != tokens.current || saved.RefreshToken == secretRefresh {
		t.Errorf("saved %q, the endpoint now expects %q", saved.RefreshToken, tokens.current)
	}
}

func TestJMAPNotCreatedIsAnError(t *testing.T) {
	backend, _, jmap := fastmailSetup(t)
	jmap.notCreated = true
	err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal})
	if err == nil || !strings.Contains(err.Error(), "overQuota") {
		t.Errorf("err = %v", err)
	}
	if backend.jmap != nil {
		t.Error("a failure should drop the cached session")
	}
}

func TestJMAPWithoutSubmissionSaysSo(t *testing.T) {
	backend, _, jmap := fastmailSetup(t)
	jmap.submission = false
	err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal})
	if err == nil || !strings.Contains(err.Error(), "does not allow sending") {
		t.Errorf("err = %v", err)
	}
}

func TestFastmailTemporaryRefusal(t *testing.T) {
	backend, tokens, _ := fastmailSetup(t)
	tokens.failure = `{"error":"invalid_grant","temporary":true}`
	err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal})
	if err == nil || !strings.Contains(err.Error(), "try again later") {
		t.Errorf("err = %v", err)
	}
}

func TestFastmailAPIToken(t *testing.T) {
	jmap := newFakeJMAP(t, func() string { return "api-SECRET" })
	backend := newTestBackend(t, emailConfig{
		Provider:    providerFastmail,
		To:          "phone@example.com",
		From:        "other@example.com",
		APIToken:    "api-SECRET",
		JMAPSession: jmap.server.URL + "/session",
	})
	if err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal}); err != nil {
		t.Fatal(err)
	}
	created := jmap.submissions[0]["create"].(map[string]any)["send"].(map[string]any)
	if created["identityId"] != "id-other" {
		t.Errorf("identity = %v, want the one for From", created["identityId"])
	}

	backend.config.APIToken = "revoked"
	backend.jmap = nil
	err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal})
	if err == nil || !strings.Contains(err.Error(), "-api-token") {
		t.Errorf("err = %v", err)
	}
}

// --- Configuration

func TestRateLimit(t *testing.T) {
	backend, _, gmail := googleSetup(t)
	now := time.Now()
	backend.now = func() time.Time { return now }
	n := Notification{Title: "x", Urgency: UrgencyNormal}
	for i := range defaultMaxPerMinute {
		if err := backend.Notify(context.Background(), n); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	err := backend.Notify(context.Background(), n)
	if err == nil || !strings.Contains(err.Error(), "max_per_minute") {
		t.Fatalf("err = %v", err)
	}
	if len(gmail.messages) != defaultMaxPerMinute {
		t.Errorf("sent %d", len(gmail.messages))
	}

	now = now.Add(61 * time.Second)
	if err := backend.Notify(context.Background(), n); err != nil {
		t.Errorf("after a minute: %v", err)
	}

	// The limit is configurable, and -1 lifts it.
	backend.config.MaxPerMinute = -1
	for range 2 * defaultMaxPerMinute {
		if err := backend.Notify(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	backend.config.MaxPerMinute = 1
	backend.recent = nil
	backend.Notify(context.Background(), n)
	if backend.Notify(context.Background(), n) == nil {
		t.Error("max_per_minute 1 allowed two")
	}
}

func TestConfigReloadsAfterAuth(t *testing.T) {
	backend, tokens, gmail := googleSetup(t)
	tokens.failure = `{"error":"invalid_grant"}`
	if backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal}) == nil {
		t.Fatal("want a failure with the revoked grant")
	}

	// `oshirase auth google` writes a new grant; the daemon picks it up.
	tokens.failure = ""
	tokens.current = "refresh-SECRET-2"
	config := *backend.config
	config.RefreshToken = "refresh-SECRET-2"
	// No pause for a distinct mtime: a new write is a new file.
	if _, err := saveEmailConfig(backend.path, &config); err != nil {
		t.Fatal(err)
	}
	if err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal}); err != nil {
		t.Fatal(err)
	}
	if len(gmail.messages) != 1 {
		t.Errorf("sent %d", len(gmail.messages))
	}
}

// The configuration directory is tightened to 0700 even when something
// else created it first, with wider permissions.
func TestSaveTightensTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "oshirase")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "email.json")
	if _, err := saveEmailConfig(path, &emailConfig{Provider: providerGoogle}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{dir: 0o700, path: 0o600} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s: mode %o, want %o", name, info.Mode().Perm(), want)
		}
	}

	// A directory that is not oshirase's own is left alone.
	other := t.TempDir()
	os.Chmod(other, 0o755)
	if _, err := saveEmailConfig(filepath.Join(other, "email.json"), &emailConfig{}); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(other); info.Mode().Perm() != 0o755 {
		t.Errorf("changed %s to %o", other, info.Mode().Perm())
	}
}

// `oshirase auth` replacing the file while the daemon refreshes the old
// grant wins: the daemon neither writes the old grant back nor misses the
// new one.
func TestAuthDuringRefreshKeepsTheNewGrant(t *testing.T) {
	backend, tokens, jmap := fastmailSetup(t)
	fresh := *backend.config
	fresh.RefreshToken = "fresh-grant-SECRET"
	tokens.hold = func() {
		tokens.hold = nil
		if _, err := saveEmailConfig(backend.path, &fresh); err != nil {
			t.Error(err)
		}
		// The old grant rotates as this refresh answers; the new grant is
		// the one the endpoint accepts next.
		tokens.rotateTo = fresh.RefreshToken
	}
	if err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal}); err != nil {
		t.Fatal(err)
	}
	if len(jmap.sent) != 1 {
		t.Errorf("sent %d", len(jmap.sent))
	}
	// The send refreshed again with the new grant, whose rotated token is
	// what the file holds now; the old grant's was never written.
	saved, _, err := loadEmailConfig(backend.path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.RefreshToken != "fresh-grant-SECRET-next" || backend.config.RefreshToken != saved.RefreshToken {
		t.Errorf("file holds %q, daemon %q; want the new grant, rotated", saved.RefreshToken, backend.config.RefreshToken)
	}
	if tokens.requests != 2 {
		t.Errorf("%d refreshes, want 2 (old grant, then new)", tokens.requests)
	}
}

// After its own save the daemon does not reload (it has nothing new to
// read), but a write by anyone else right after is still seen.
func TestOwnSaveIsNotAChange(t *testing.T) {
	backend, _, _ := fastmailSetup(t)
	if err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal}); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(backend.path)
	if !sameVersion(backend.loaded, info) {
		t.Error("the daemon's own save counts as a change")
	}
	other := *backend.config
	other.RefreshToken = "other"
	saveEmailConfig(backend.path, &other)
	info, _ = os.Stat(backend.path)
	if sameVersion(backend.loaded, info) {
		t.Error("another write right after the daemon's is not seen")
	}
}

func TestMissingConfigSaysWhatToRun(t *testing.T) {
	t.Setenv("OSHIRASE_EMAIL_CONFIG", filepath.Join(t.TempDir(), "none.json"))
	_, err := selectBackend("email")
	if err == nil || !strings.Contains(err.Error(), "run oshirase auth") {
		t.Errorf("err = %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	cases := map[string]emailConfig{
		"no provider":         {To: "a@example.com"},
		"bad to":              {Provider: providerGoogle, To: "nope", ClientID: "c", ClientSecret: "s", RefreshToken: "r", TokenEndpoint: "t"},
		"google no secret":    {Provider: providerGoogle, To: "a@example.com", ClientID: "c", RefreshToken: "r", TokenEndpoint: "t"},
		"fastmail no from":    {Provider: providerFastmail, To: "a@example.com", APIToken: "x", JMAPSession: "s"},
		"fastmail no grant":   {Provider: providerFastmail, To: "a@example.com", From: "b@example.com", JMAPSession: "s"},
		"fastmail no session": {Provider: providerFastmail, To: "a@example.com", From: "b@example.com", APIToken: "x"},
		"negative limit":      {Provider: providerFastmail, To: "a@example.com", From: "b@example.com", APIToken: "x", JMAPSession: "s", MaxPerMinute: -2},
	}
	for name, config := range cases {
		if config.validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFastmailRefusesTheResource(t *testing.T) {
	backend, tokens, _ := fastmailSetup(t)
	tokens.failure = `{"error":"invalid_target"}`
	err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal})
	if err == nil || !strings.Contains(err.Error(), "check jmap_session") {
		t.Errorf("err = %v", err)
	}
	if backend.config.RefreshToken != secretRefresh {
		t.Error("a refused refresh changed the stored refresh token")
	}
}
