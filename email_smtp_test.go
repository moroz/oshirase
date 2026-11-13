package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const secretSMTPPassword = "app-SECRET-password"

// testCertificate is a self-signed certificate for 127.0.0.1, and a pool
// that trusts it.
func testCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake smtp"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed}, pool
}

// fakeSMTP is an SMTP server over TLS, in process, that records what it
// was told. implicit makes it TLS from the first byte (port 465); without
// it, it offers STARTTLS unless noSTARTTLS.
type fakeSMTP struct {
	t        *testing.T
	listener net.Listener
	tls      *tls.Config
	roots    *x509.CertPool
	implicit bool

	// Under mu, since the race detector does not count the network as
	// synchronization: what tests set, and what the server records.
	mu         sync.Mutex
	noSTARTTLS bool
	password   string   // the one it accepts
	stall      bool     // never greet
	echoAuth   bool     // quote the credentials back in a refusal
	commands   []string // verbs, in order
	logins     []string // username:password, as received
	plainAuth  bool     // AUTH arrived without TLS
	from, to   []string
	messages   []string
}

func newFakeSMTP(t *testing.T, implicit bool) *fakeSMTP {
	t.Helper()
	certificate, roots := testCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{
		t: t, listener: listener, roots: roots, implicit: implicit,
		tls:      &tls.Config{Certificates: []tls.Certificate{certificate}},
		password: secretSMTPPassword,
	}
	var running sync.WaitGroup
	t.Cleanup(func() { listener.Close(); running.Wait() })
	running.Add(1)
	go func() {
		defer running.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			running.Add(1)
			go func() {
				defer running.Done()
				defer conn.Close()
				f.serve(conn)
			}()
		}
	}()
	return f
}

// transport reaches the fake whatever host:port the configuration names,
// so a test can say :465 or :587 and trusts its certificate.
func (f *fakeSMTP) transport() smtpTransport {
	return smtpTransport{
		roots: f.roots,
		dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, f.listener.Addr().String())
		},
	}
}

// set changes the fake's behaviour.
func (f *fakeSMTP) set(change func(*fakeSMTP)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

// state is a copy of what the fake has recorded.
type fakeSMTPState struct {
	commands, logins, from, to, messages []string
	plainAuth                            bool
}

func (f *fakeSMTP) state() fakeSMTPState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fakeSMTPState{
		commands: slices.Clone(f.commands), logins: slices.Clone(f.logins),
		from: slices.Clone(f.from), to: slices.Clone(f.to),
		messages: slices.Clone(f.messages), plainAuth: f.plainAuth,
	}
}

func (f *fakeSMTP) record(verb string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, verb)
}

func (f *fakeSMTP) saw(verb string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, command := range f.commands {
		if command == verb {
			return true
		}
	}
	return false
}

func (f *fakeSMTP) serve(conn net.Conn) {
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	f.mu.Lock()
	stall, noSTARTTLS, password, echoAuth := f.stall, f.noSTARTTLS, f.password, f.echoAuth
	f.mu.Unlock()
	if stall {
		io.Copy(io.Discard, conn) // until the client gives up
		return
	}
	secure := false
	if f.implicit {
		conn = tls.Server(conn, f.tls)
		secure = true
	}
	text := textproto.NewConn(conn)
	reply := func(format string, args ...any) { text.PrintfLine(format, args...) }
	reply("220 fake ESMTP")
	for {
		line, err := text.ReadLine()
		if err != nil {
			return
		}
		verb, argument, _ := strings.Cut(line, " ")
		verb = strings.ToUpper(verb)
		f.record(verb)
		switch verb {
		case "EHLO", "HELO":
			lines := []string{"fake"}
			if !secure && !noSTARTTLS {
				lines = append(lines, "STARTTLS")
			}
			lines = append(lines, "AUTH PLAIN")
			for i, l := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				reply("250%s%s", sep, l)
			}
		case "STARTTLS":
			if secure || noSTARTTLS {
				reply("502 no")
				continue
			}
			reply("220 go ahead")
			conn = tls.Server(conn, f.tls)
			text = textproto.NewConn(conn)
			secure = true
		case "AUTH":
			f.mu.Lock()
			if !secure {
				f.plainAuth = true
			}
			f.mu.Unlock()
			mechanism, initial, _ := strings.Cut(argument, " ")
			decoded, _ := base64.StdEncoding.DecodeString(initial)
			parts := strings.Split(string(decoded), "\x00")
			if strings.ToUpper(mechanism) != "PLAIN" || len(parts) != 3 {
				reply("504 unsupported")
				continue
			}
			f.mu.Lock()
			f.logins = append(f.logins, parts[1]+":"+parts[2])
			f.mu.Unlock()
			if parts[2] == password {
				reply("235 2.7.0 ok")
			} else if echoAuth {
				reply("535 5.7.8 Rejected %s for %s (%s)", initial, parts[1], parts[2])
			} else {
				reply("535 5.7.8 Authentication failed for %s", parts[1])
			}
		case "MAIL":
			f.mu.Lock()
			f.from = append(f.from, argument)
			f.mu.Unlock()
			reply("250 ok")
		case "RCPT":
			f.mu.Lock()
			f.to = append(f.to, argument)
			f.mu.Unlock()
			reply("250 ok")
		case "DATA":
			reply("354 go on")
			data, err := text.ReadDotBytes()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.messages = append(f.messages, string(data))
			f.mu.Unlock()
			reply("250 queued")
		case "RSET", "NOOP":
			reply("250 ok")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 unknown")
		}
	}
}

func smtpConfig(host string) emailConfig {
	return emailConfig{
		Provider: providerSMTP, To: "phone@example.com", From: "me@fastmail.com",
		SMTPHost: host, Username: "me@fastmail.com", SMTPPassword: secretSMTPPassword,
	}
}

func smtpSetup(t *testing.T, implicit bool, host string) (*emailBackend, *fakeSMTP) {
	t.Helper()
	server := newFakeSMTP(t, implicit)
	backend := newTestBackend(t, smtpConfig(host))
	backend.smtpNet = server.transport()
	return backend, server
}

func TestSMTPImplicitTLSSends(t *testing.T) {
	logs := captureLog(t)
	backend, server := smtpSetup(t, true, "127.0.0.1:465")
	n := Notification{Title: "Touch the key", Body: secretBody, Source: "ssh", Urgency: UrgencyCritical}
	if err := backend.Notify(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	got := server.state()
	if len(got.messages) != 1 {
		t.Fatalf("%d messages", len(got.messages))
	}
	if got.logins[0] != "me@fastmail.com:"+secretSMTPPassword {
		t.Errorf("login = %q", got.logins[0])
	}
	if got.from[0] != "FROM:<me@fastmail.com>" || got.to[0] != "TO:<phone@example.com>" {
		t.Errorf("envelope = %q, %q", got.from, got.to)
	}
	if server.saw("STARTTLS") {
		t.Error("STARTTLS on the implicit TLS port")
	}
	if !server.saw("QUIT") {
		t.Error("no QUIT")
	}

	message, err := mail.ReadMessage(strings.NewReader(got.messages[0]))
	if err != nil {
		t.Fatal(err)
	}
	header := message.Header
	subject, _ := new(mime.WordDecoder).DecodeHeader(header.Get("Subject"))
	checks := map[string]string{
		"From":       `"oshirase" <me@fastmail.com>`,
		"To":         "<phone@example.com>",
		"Importance": "high",
		"X-Priority": "1 (Highest)",
	}
	for name, want := range checks {
		if got := header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if subject != "[oshirase · ssh] Touch the key" {
		t.Errorf("Subject = %q", subject)
	}
	if !regexp.MustCompile(`^<[0-9a-f]{32}@fastmail\.com>$`).MatchString(header.Get("Message-ID")) {
		t.Errorf("Message-ID = %q", header.Get("Message-ID"))
	}
	if date, err := header.Date(); err != nil || time.Since(date) > time.Minute {
		t.Errorf("Date = %q (%v)", header.Get("Date"), err)
	}
	body, _ := io.ReadAll(message.Body)
	if !strings.Contains(string(body), secretBody) {
		t.Errorf("body = %q", body)
	}
	assertNoSecrets(t, "log", logs.String())

	// Each message has its own Message-ID.
	backend.Notify(context.Background(), n)
	second, _ := mail.ReadMessage(strings.NewReader(server.state().messages[1]))
	if second.Header.Get("Message-ID") == header.Get("Message-ID") {
		t.Error("Message-ID repeated")
	}
}

func TestSMTPStartTLSSends(t *testing.T) {
	backend, server := smtpSetup(t, false, "127.0.0.1:587")
	backend.config.Username = "" // defaults to from
	if err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyLow}); err != nil {
		t.Fatal(err)
	}
	got := server.state()
	if !server.saw("STARTTLS") || got.plainAuth {
		t.Errorf("commands %v, plain auth %v", got.commands, got.plainAuth)
	}
	if len(got.messages) != 1 || got.logins[0] != "me@fastmail.com:"+secretSMTPPassword {
		t.Fatalf("messages %d, logins %q", len(got.messages), got.logins)
	}
	message, _ := mail.ReadMessage(strings.NewReader(got.messages[0]))
	if message.Header.Get("X-Priority") != "5 (Lowest)" {
		t.Errorf("X-Priority = %q", message.Header.Get("X-Priority"))
	}
}

func TestSMTPWithoutStartTLSIsRefusedBeforeAuth(t *testing.T) {
	backend, server := smtpSetup(t, false, "127.0.0.1:587")
	server.set(func(f *fakeSMTP) { f.noSTARTTLS = true })
	err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal})
	if err == nil || !strings.Contains(err.Error(), "does not offer STARTTLS") || !strings.Contains(err.Error(), "127.0.0.1:587") {
		t.Fatalf("err = %v", err)
	}
	if got := server.state(); server.saw("AUTH") || len(got.logins) != 0 {
		t.Errorf("logged in without TLS: %v", got.commands)
	}
}

func TestSMTPRefusedPassword(t *testing.T) {
	logs := captureLog(t)
	backend, server := smtpSetup(t, true, "127.0.0.1:465")
	server.set(func(f *fakeSMTP) { f.password = "another" })
	err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal})
	if err == nil {
		t.Fatal("sent with a refused password")
	}
	for _, want := range []string{"refused the password", "535", "make a new app password", "oshirase auth smtp", "127.0.0.1:465"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v; want %q in it", err, want)
		}
	}
	assertNoSecrets(t, "error", err.Error())
	assertNoSecrets(t, "log", logs.String())
	if server.saw("MAIL") || server.saw("DATA") {
		t.Error("went on after the refusal")
	}
}

// The certificate is checked against the host name: without the test's
// root, the system's roots do not trust it, and nothing is sent.
func TestSMTPVerifiesTheCertificate(t *testing.T) {
	for _, implicit := range []bool{true, false} {
		host := "127.0.0.1:587"
		if implicit {
			host = "127.0.0.1:465"
		}
		backend, server := smtpSetup(t, implicit, host)
		backend.smtpNet.roots = nil
		err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal})
		if err == nil || !strings.Contains(err.Error(), "certificate") || !strings.Contains(err.Error(), host) {
			t.Errorf("%s: err = %v", host, err)
		}
		if len(server.state().logins) != 0 {
			t.Errorf("%s: logged in to an unverified server", host)
		}
	}
}

func TestSMTPHonoursTheDeadline(t *testing.T) {
	backend, server := smtpSetup(t, true, "127.0.0.1:465")
	server.set(func(f *fakeSMTP) { f.stall = true })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := backend.Notify(ctx, Notification{Title: "x", Urgency: UrgencyNormal})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("took %s", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "did not answer in time") {
		t.Errorf("err = %v", err)
	}
}

func TestSMTPConnectionFailureNamesTheHost(t *testing.T) {
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	address := listener.Addr().String()
	listener.Close() // nothing listens there now
	backend := newTestBackend(t, smtpConfig(address))
	err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal})
	if err == nil || !strings.Contains(err.Error(), "connecting to the SMTP server "+address) {
		t.Errorf("err = %v", err)
	}
}

func TestSMTPRateLimit(t *testing.T) {
	backend, server := smtpSetup(t, true, "127.0.0.1:465")
	backend.config.MaxPerMinute = 1
	n := Notification{Title: "x", Urgency: UrgencyNormal}
	if err := backend.Notify(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if err := backend.Notify(context.Background(), n); err == nil || !strings.Contains(err.Error(), "max_per_minute") {
		t.Errorf("err = %v", err)
	}
	if sent := len(server.state().messages); sent != 1 {
		t.Errorf("sent %d", sent)
	}
}

// A daemon running on a Fastmail grant switches to SMTP when auth rewrites
// email.json, and reads the password from the new keyring item.
func TestSMTPReloadFromTheKeyring(t *testing.T) {
	backend, _, jmap := keyringFastmailSetup(t)
	if err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal}); err != nil {
		t.Fatal(err)
	}
	server := newFakeSMTP(t, true)
	backend.smtpNet = server.transport()
	config := smtpConfig("127.0.0.1:465")
	if err := storeGrant(context.Background(), backend.path, &config, secretsInKeyring, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := backend.Notify(context.Background(), Notification{Title: "y", Urgency: UrgencyNormal}); err != nil {
		t.Fatal(err)
	}
	if sent := len(server.state().messages); sent != 1 || len(jmap.sent) != 1 {
		t.Errorf("smtp %d, jmap %d", sent, len(jmap.sent))
	}
	if _, ok := keyringItem(t, "grant-a"); ok {
		t.Error("the Fastmail grant's item is still there")
	}
}

func TestSMTPConfigValidation(t *testing.T) {
	good := smtpConfig("smtp.fastmail.com:465")
	if err := good.validate(); err != nil {
		t.Errorf("good: %v", err)
	}
	cases := map[string]func(*emailConfig){
		"no host":      func(c *emailConfig) { c.SMTPHost = "" },
		"no port":      func(c *emailConfig) { c.SMTPHost = "smtp.fastmail.com" },
		"bad port":     func(c *emailConfig) { c.SMTPHost = "smtp.fastmail.com:smtp" },
		"no from":      func(c *emailConfig) { c.From = "" },
		"no password":  func(c *emailConfig) { c.SMTPPassword = "" },
		"bad to":       func(c *emailConfig) { c.To = "nope" },
		"empty host":   func(c *emailConfig) { c.SMTPHost = ":465" },
		"port too big": func(c *emailConfig) { c.SMTPHost = "smtp.fastmail.com:70000" },
	}
	for name, change := range cases {
		config := good
		change(&config)
		if config.validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// In keyring mode the file holds no password, and that is its shape.
	keyed := good
	keyed.SMTPPassword, keyed.Secrets, keyed.GrantID = "", secretsInKeyring, "g"
	if err := keyed.validateShape(); err != nil {
		t.Errorf("keyring mode: %v", err)
	}
	keyed.SMTPPassword = "x"
	if keyed.validateShape() == nil {
		t.Error("keyring mode with the password in the file: accepted")
	}
	// The configuration saved in keyring mode drops the password.
	keyed.SMTPPassword = secretSMTPPassword
	path := writeConfig(t, keyed)
	saved, _, err := loadEmailConfig(path)
	if err != nil || saved.SMTPPassword != "" {
		t.Errorf("saved %+v, %v", saved, err)
	}
	if !strings.Contains(fmt.Sprint(bareAddress(`"Me" <me@example.com>`)), "me@example.com") {
		t.Error("bareAddress")
	}
}

// A line holding only a dot is dot-stuffed on the way, not taken for the
// end of the message.
func TestSMTPBodyWithLeadingDot(t *testing.T) {
	backend, server := smtpSetup(t, true, "127.0.0.1:465")
	if err := backend.Notify(context.Background(), Notification{Title: "x", Body: "one\n.\ntwo", Urgency: UrgencyNormal}); err != nil {
		t.Fatal(err)
	}
	message, _ := mail.ReadMessage(strings.NewReader(server.state().messages[0]))
	body, _ := io.ReadAll(bufio.NewReader(message.Body))
	// The fake's ReadDotBytes turns CRLF into LF.
	if !strings.Contains(string(body), "one\n.\ntwo") {
		t.Errorf("body = %q", body)
	}
}

// A server that quotes the credentials back in its refusal, base64 or
// plain, does not get the password into the error.
func TestSMTPRefusalQuotingThePasswordIsRedacted(t *testing.T) {
	backend, server := smtpSetup(t, true, "127.0.0.1:465")
	server.set(func(f *fakeSMTP) { f.password, f.echoAuth = "another", true })
	err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal})
	if err == nil || !strings.Contains(err.Error(), "refused the password") {
		t.Fatalf("err = %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("\x00me@fastmail.com\x00" + secretSMTPPassword))
	if strings.Contains(err.Error(), encoded) || !strings.Contains(err.Error(), "[password]") {
		t.Errorf("err = %v", err)
	}
	assertNoSecrets(t, "error", err.Error())
}

// --- oshirase auth smtp

// authSMTPSetup is a fake SMTP server and a configuration path that holds
// a Fastmail OAuth grant in the keyring (item grant-old), as before
// switching to SMTP.
func authSMTPSetup(t *testing.T) (*fakeSMTP, string, []byte) {
	t.Helper()
	freshKeyring(t)
	server := newFakeSMTP(t, true)
	old := emailConfig{
		Provider: providerFastmail, To: "phone@example.com", From: "me@fastmail.com",
		TokenEndpoint: "https://token.example", ClientID: "dyn-client", JMAPSession: "https://jmap.example/session",
		Secrets: secretsInKeyring, GrantID: "grant-old",
	}
	path := writeConfig(t, old)
	if err := keyringStore.set(context.Background(), "grant-old", emailSecrets{RefreshToken: secretRefresh}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	return server, path, before
}

func TestAuthSMTPStoresInTheKeyring(t *testing.T) {
	logs := captureLog(t)
	server, path, _ := authSMTPSetup(t)
	var report strings.Builder
	args := []string{"-host", "127.0.0.1:465", "-username", "me@fastmail.com", "-to", "phone@example.com", "-config", path}
	if err := authSMTP(context.Background(), args, strings.NewReader(secretSMTPPassword+"\n"), &report, server.transport()); err != nil {
		t.Fatal(err)
	}

	// The probe logged in and out, and sent nothing.
	got := server.state()
	if !slices.Equal(got.commands, []string{"EHLO", "AUTH", "QUIT"}) {
		t.Errorf("probe commands = %v", got.commands)
	}
	if len(got.logins) != 1 || got.logins[0] != "me@fastmail.com:"+secretSMTPPassword {
		t.Errorf("logins = %q", got.logins)
	}

	data, _ := os.ReadFile(path)
	assertNoSecrets(t, "email.json", string(data))
	assertNoSecrets(t, "report", report.String())
	assertNoSecrets(t, "log", logs.String())
	saved, _, err := loadEmailConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Provider != providerSMTP || saved.From != "me@fastmail.com" || saved.SMTPHost != "127.0.0.1:465" ||
		!saved.inKeyring() || saved.GrantID == "grant-old" {
		t.Errorf("saved = %+v", saved)
	}
	if item, ok := keyringItem(t, saved.GrantID); !ok || item != (emailSecrets{SMTPPassword: secretSMTPPassword}) {
		t.Errorf("item = %+v, %v", item, ok)
	}
	if _, ok := keyringItem(t, "grant-old"); ok {
		t.Error("the Fastmail grant's item is still there")
	}
	for _, want := range []string{"Saved the SMTP password", keyringUser(saved.GrantID), path, "revoke it in Fastmail's settings"} {
		if !strings.Contains(report.String(), want) {
			t.Errorf("report lacks %q: %s", want, report.String())
		}
	}

	// The daemon sends with what was stored.
	backend, err := newEmailBackend(path)
	if err != nil {
		t.Fatal(err)
	}
	backend.smtpNet = server.transport()
	if err := backend.Notify(context.Background(), Notification{Title: "x", Urgency: UrgencyNormal}); err != nil {
		t.Fatal(err)
	}
	if sent := len(server.state().messages); sent != 1 {
		t.Errorf("sent %d", sent)
	}
}

func TestAuthSMTPProbeFailureSavesNothing(t *testing.T) {
	server, path, before := authSMTPSetup(t)
	server.set(func(f *fakeSMTP) { f.password = "another" })
	var report strings.Builder
	args := []string{"-host", "127.0.0.1:465", "-username", "me@fastmail.com", "-to", "phone@example.com", "-config", path}
	err := authSMTP(context.Background(), args, strings.NewReader(secretSMTPPassword+"\n"), &report, server.transport())
	if err == nil || !strings.Contains(err.Error(), "nothing saved") || !strings.Contains(err.Error(), "535") {
		t.Fatalf("err = %v", err)
	}
	assertNoSecrets(t, "error", err.Error())
	assertNoSecrets(t, "report", report.String())
	if server.saw("MAIL") || server.saw("DATA") {
		t.Errorf("the probe went on to send: %v", server.state().commands)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("email.json changed:\n%s", after)
	}
	if _, ok := keyringItem(t, "grant-old"); !ok {
		t.Error("the previous grant's item was deleted")
	}
}

func TestAuthSMTPInAFile(t *testing.T) {
	freshKeyring(t)
	server := newFakeSMTP(t, false)
	path := filepath.Join(t.TempDir(), "oshirase", "email.json")
	var report strings.Builder
	args := []string{"-host", "127.0.0.1:587", "-username", "login-name", "-from", "Me <me@example.com>",
		"-to", "phone@example.com", "-config", path, "-store", "file"}
	// A password manager's pipe, with spaces in the password and a CRLF.
	password := " spaced SECRET "
	server.set(func(f *fakeSMTP) { f.password = password })
	if err := authSMTP(context.Background(), args, strings.NewReader(password+"\r\n"), &report, server.transport()); err != nil {
		t.Fatal(err)
	}
	if got := server.state(); !server.saw("STARTTLS") || got.plainAuth || got.logins[0] != "login-name:"+password {
		t.Errorf("commands %v, logins %q", got.commands, got.logins)
	}
	info, _ := os.Stat(path)
	saved, _, err := loadEmailConfig(path)
	if err != nil || info.Mode().Perm() != 0o600 || saved.SMTPPassword != password || saved.Username != "login-name" || saved.From != "Me <me@example.com>" {
		t.Errorf("saved %+v (mode %o), %v", saved, info.Mode().Perm(), err)
	}
	assertNoSecrets(t, "report", report.String())
	if !strings.Contains(report.String(), "everything in "+path) {
		t.Errorf("report = %s", report.String())
	}
}

func TestAuthSMTPFlags(t *testing.T) {
	freshKeyring(t)
	path := filepath.Join(t.TempDir(), "email.json")
	cases := map[string]struct {
		args []string
		want string
	}{
		"no host":           {[]string{"-username", "me@example.com", "-to", "p@example.com"}, "-host"},
		"no port":           {[]string{"-host", "smtp.example.com", "-username", "me@example.com", "-to", "p@example.com"}, "-host"},
		"username not addr": {[]string{"-host", "smtp.example.com:465", "-username", "me", "-to", "p@example.com"}, "give -from"},
		"no to":             {[]string{"-host", "smtp.example.com:465", "-username", "me@example.com"}, "-to is required"},
		"bad store":         {[]string{"-host", "smtp.example.com:465", "-username", "me@example.com", "-to", "p@example.com", "-store", "x"}, "-store"},
		"password as arg":   {[]string{"-host", "smtp.example.com:465", "-username", "me@example.com", "-to", "p@example.com", secretSMTPPassword}, "never from the command line"},
	}
	for name, c := range cases {
		var report strings.Builder
		transport := smtpTransport{dial: func(context.Context, string, string) (net.Conn, error) {
			t.Errorf("%s: connected", name)
			return nil, errors.New("no")
		}}
		args := append(c.args, "-config", path)
		if c.args[len(c.args)-1] == secretSMTPPassword {
			args = append([]string{"-config", path}, c.args...)
		}
		err := authSMTP(context.Background(), args, strings.NewReader(secretSMTPPassword+"\n"), &report, transport)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v", name, err)
			continue
		}
		assertNoSecrets(t, name+": error", err.Error())
		assertNoSecrets(t, name+": report", report.String())
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a configuration was written")
	}
}

func TestReadSMTPPassword(t *testing.T) {
	for input, want := range map[string]string{"pw\n": "pw", "pw\r\n": "pw", "pw": "pw", "a b \n": "a b ", "first\nsecond\n": "first"} {
		got, err := readSMTPPassword(strings.NewReader(input), io.Discard, "u", "h:465")
		if err != nil || got != want {
			t.Errorf("%q: %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"", "\n"} {
		if _, err := readSMTPPassword(strings.NewReader(input), io.Discard, "u", "h:465"); err == nil {
			t.Errorf("%q: accepted", input)
		}
	}
}

// The daemon's real path for an SMTP grant: GET /v1/health lists email,
// a notification with via: email reaches the SMTP server, the rate limit
// answers instead of sending, and a new password from oshirase auth smtp
// is picked up without a restart.
func TestSMTPViaEmailHealthAndReload(t *testing.T) {
	freshKeyring(t)
	logs := captureLog(t)
	server := newFakeSMTP(t, true)
	path := filepath.Join(t.TempDir(), "oshirase", "email.json")
	t.Setenv("OSHIRASE_EMAIL_CONFIG", path)
	auth := func(password string) {
		t.Helper()
		args := []string{"-host", "127.0.0.1:465", "-username", "me@fastmail.com", "-to", "phone@example.com"}
		if err := authSMTP(context.Background(), args, strings.NewReader(password+"\n"), io.Discard, server.transport()); err != nil {
			t.Fatal(err)
		}
	}
	auth(secretSMTPPassword)
	// Two a minute, so the third is refused.
	config, _, _ := loadEmailConfig(path)
	config.MaxPerMinute = 2
	saveEmailConfig(path, config)

	screen := &recordingBackend{name: "freedesktop"}
	routes := newRouter(screen)
	routes.openEmail = func() (Backend, error) {
		backend, err := openEmailBackend()
		if err == nil {
			backend.(*emailBackend).smtpNet = server.transport()
		}
		return backend, err
	}

	recorder := httptest.NewRecorder()
	healthHandler(routes)(recorder, httptest.NewRequest(http.MethodGet, "/v1/health", nil))
	var health HealthResponse
	json.Unmarshal(recorder.Body.Bytes(), &health)
	if !slices.Equal(health.Via, []string{ViaEmail}) {
		t.Errorf("health via = %q", health.Via)
	}

	if got := notify(t, routes, `{"title":"first","via":"email"}`); got.Code != http.StatusOK {
		t.Fatalf("%d %s", got.Code, got.Body)
	}
	if got := notify(t, routes, `{"title":"on screen"}`); got.Code != http.StatusOK || len(screen.got) != 1 {
		t.Fatalf("%d %s", got.Code, got.Body)
	}

	// A new app password: the old one stops working, auth stores the new
	// one in a new keyring item, and the daemon reloads.
	const newPassword = "second-SECRET-password"
	server.set(func(f *fakeSMTP) { f.password = newPassword })
	auth(newPassword)
	config, _, _ = loadEmailConfig(path)
	config.MaxPerMinute = 2
	saveEmailConfig(path, config)
	if got := notify(t, routes, `{"title":"second","via":"email"}`); got.Code != http.StatusOK {
		t.Fatalf("after re-auth: %d %s", got.Code, got.Body)
	}
	if got := notify(t, routes, `{"title":"third","via":"email"}`); got.Code == http.StatusOK || !strings.Contains(got.Body.String(), "max_per_minute") {
		t.Errorf("over the limit: %d %s", got.Code, got.Body)
	}

	state := server.state()
	if len(state.messages) != 2 || state.logins[len(state.logins)-1] != "me@fastmail.com:"+newPassword {
		t.Errorf("messages %d, logins %q", len(state.messages), state.logins)
	}
	if !strings.Contains(logs.String(), "reloaded "+path+" (provider smtp)") {
		t.Errorf("no reload in the log: %s", logs.String())
	}
	assertNoSecrets(t, "log", logs.String())
}
