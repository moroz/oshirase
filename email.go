package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The email backend delivers each notification as an email, for being
// reached away from the host's screen. It runs on any platform: nothing in
// it depends on the OS, only on the configuration that `oshirase auth`
// writes.

const (
	providerGoogle   = "google"
	providerFastmail = "fastmail"
)

// emailConfig is the file `oshirase auth` writes and the daemon reads. The
// secrets in it (refresh_token, api_token, client_secret) are in the file
// only when Secrets is "file" or empty; with "keyring" they are in the OS
// keyring and the daemon fills them in from there. Either way the file is
// written with mode 0600 in a directory with mode 0700.
type emailConfig struct {
	Provider string `json:"provider"` // google or fastmail
	To       string `json:"to"`
	// From is required for Fastmail (it picks the sending identity, and
	// `oshirase auth fastmail` fills it in) and optional for Google, where
	// Gmail sends as the authorized account.
	From string `json:"from,omitempty"`

	// OAuth: the client the grant was made to and the refresh token from
	// it. ClientSecret is empty for Fastmail, whose dynamically
	// registered clients are public.
	TokenEndpoint string `json:"token_endpoint,omitempty"`
	ClientID      string `json:"client_id,omitempty"`
	ClientSecret  string `json:"client_secret,omitempty"`
	RefreshToken  string `json:"refresh_token,omitempty"`

	// APIToken replaces OAuth for Fastmail when set: a token made in
	// Fastmail's Settings → Privacy & Security.
	APIToken string `json:"api_token,omitempty"`

	// JMAPSession is the JMAP session URL (Fastmail).
	JMAPSession string `json:"jmap_session,omitempty"`

	// MaxPerMinute caps how many emails the daemon sends in any minute: 0
	// means the default, defaultMaxPerMinute, and -1 means no limit.
	MaxPerMinute int `json:"max_per_minute,omitempty"`

	// Secrets is where the secrets are: "keyring", "file", or empty for a
	// configuration from before the keyring (the file). GrantID names the
	// keyring item; see keyringUser.
	Secrets string `json:"secrets,omitempty"`
	GrantID string `json:"grant_id,omitempty"`
}

// defaultMaxPerMinute keeps whatever can reach the daemon from sending
// unlimited mail from the owner's account. A person wants a few
// notifications a minute at most; a loop gone wrong wants hundreds.
const defaultMaxPerMinute = 10

func (c *emailConfig) perMinute() int {
	if c.MaxPerMinute == 0 {
		return defaultMaxPerMinute
	}
	return c.MaxPerMinute
}

// validateShape checks what the file says, without the secrets, which in
// keyring mode are not in it.
func (c *emailConfig) validateShape() error {
	switch c.Secrets {
	case "", secretsInFile:
	case secretsInKeyring:
		if c.GrantID == "" {
			return errors.New(`"secrets": "keyring" needs grant_id`)
		}
		if c.secrets() != (emailSecrets{}) {
			return errors.New(`"secrets" is "keyring" but the file holds secrets too; remove them, or run oshirase auth again`)
		}
	default:
		return fmt.Errorf(`secrets must be "keyring" or "file", not %q`, c.Secrets)
	}
	if !c.inKeyring() {
		return c.validate()
	}
	// Stand-ins for the secrets, so the rest is checked as for a file.
	full := *c
	if c.Provider == providerFastmail && c.ClientID == "" {
		full.APIToken = "-"
	} else {
		full.RefreshToken, full.ClientSecret = "-", "-"
	}
	return full.validate()
}

// validate checks the configuration with its secrets in place.
func (c *emailConfig) validate() error {
	switch c.Provider {
	case providerGoogle:
		if c.ClientID == "" || c.ClientSecret == "" || c.RefreshToken == "" || c.TokenEndpoint == "" {
			return errors.New("google needs client_id, client_secret, token_endpoint and refresh_token")
		}
	case providerFastmail:
		if c.JMAPSession == "" {
			return errors.New("fastmail needs jmap_session")
		}
		if c.APIToken == "" && (c.ClientID == "" || c.RefreshToken == "" || c.TokenEndpoint == "") {
			return errors.New("fastmail needs either api_token or client_id, token_endpoint and refresh_token")
		}
		if c.From == "" {
			return errors.New("fastmail needs from, the address to send as")
		}
	default:
		return fmt.Errorf("provider must be google or fastmail, not %q", c.Provider)
	}
	if c.MaxPerMinute < -1 {
		return fmt.Errorf("max_per_minute must be a positive number, 0 for the default (%d) or -1 for no limit", defaultMaxPerMinute)
	}
	if _, err := mail.ParseAddress(c.To); err != nil {
		return fmt.Errorf("to %q is not an email address: %w", c.To, err)
	}
	if c.From != "" {
		if _, err := mail.ParseAddress(c.From); err != nil {
			return fmt.Errorf("from %q is not an email address: %w", c.From, err)
		}
	}
	return nil
}

// emailConfigPath is $OSHIRASE_EMAIL_CONFIG, else email.json in the
// platform's configuration directory: $XDG_CONFIG_HOME/oshirase (or
// ~/.config/oshirase) on Linux, ~/Library/Application Support/oshirase on
// macOS.
func emailConfigPath() (string, error) {
	if path := os.Getenv("OSHIRASE_EMAIL_CONFIG"); path != "" {
		return path, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding the configuration directory: %w; set OSHIRASE_EMAIL_CONFIG", err)
	}
	return filepath.Join(dir, "oshirase", "email.json"), nil
}

// loadEmailConfig also returns what the file was when read: the stat of
// the open file, so it describes the content decoded and not whatever
// replaced it a moment later.
func loadEmailConfig(path string) (*emailConfig, os.FileInfo, error) {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, fmt.Errorf("no email configuration at %s; run oshirase auth google or oshirase auth fastmail", path)
	}
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	var config emailConfig
	decoder := json.NewDecoder(file)
	if err := decoder.Decode(&config); err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := config.validateShape(); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	if !config.inKeyring() && info.Mode().Perm()&0o077 != 0 {
		log.Printf("warning: %s is readable by others (mode %o); it holds a credential, chmod 600 it", path, info.Mode().Perm())
	}
	return &config, info, nil
}

// saveEmailConfig replaces the file atomically, so a daemon reading it, or
// a crash halfway, never sees half a refresh token. It returns what it
// wrote, taken from the temporary file before the rename (which keeps the
// inode and the modification time): a stat of the path after the rename
// could already be describing someone else's write.
func saveEmailConfig(path string, config *emailConfig) (os.FileInfo, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// MkdirAll leaves an existing directory as it is, and an installer may
	// have made it 0755 for other files. Tighten it when it is oshirase's
	// own; a directory named in $OSHIRASE_EMAIL_CONFIG (a home directory,
	// say) is the person's business, and the file is 0600 regardless.
	if filepath.Base(dir) == "oshirase" {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, err
		}
	}
	stored := *config
	if stored.inKeyring() {
		stored.setSecrets(emailSecrets{}) // never in the file in keyring mode
	}
	data, err := json.MarshalIndent(&stored, "", "  ")
	if err != nil {
		return nil, err
	}
	temp, err := os.CreateTemp(dir, ".email-*.json")
	if err != nil {
		return nil, err
	}
	defer os.Remove(temp.Name()) // a no-op once renamed
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return nil, err
	}
	if _, err := temp.Write(append(data, '\n')); err != nil {
		temp.Close()
		return nil, err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return nil, err
	}
	written, err := temp.Stat()
	if err != nil {
		temp.Close()
		return nil, err
	}
	if err := temp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return nil, err
	}
	return written, nil
}

// openEmailBackend is what -backend email selects. Unlike the macOS
// backend it refuses to start without its configuration: there is nothing
// it could deliver to, and the error says what to run. A keyring it cannot
// read is different: that can come right (an unlock), so the daemon starts,
// logs why, and answers 502 with the same reason until it does.
func openEmailBackend() (Backend, error) {
	path, err := emailConfigPath()
	if err != nil {
		return nil, err
	}
	return newEmailBackend(path)
}

func newEmailBackend(path string) (*emailBackend, error) {
	config, loaded, err := loadEmailConfig(path)
	if err != nil {
		return nil, err
	}
	backend := &emailBackend{
		path:         path,
		config:       config,
		loaded:       loaded,
		http:         &http.Client{},
		gmailSendURL: gmailSendURL,
		secretsReady: !config.inKeyring(),
		keys:         keyringStore,
	}
	if err := backend.ensureSecrets(context.Background()); err != nil {
		log.Printf("warning: email: %v", err)
	}
	return backend, nil
}

// refreshTimeout bounds a token refresh, which is not cut short when the
// notification's own request is; see accessToken.
const refreshTimeout = 15 * time.Second

type emailBackend struct {
	path         string
	http         *http.Client
	gmailSendURL string
	now          func() time.Time // tests replace it
	// ephemeral keeps a rotated refresh token in config only, never in the
	// file: for `oshirase auth`, which probes a grant it has not saved yet
	// and saves the whole configuration itself afterwards.
	ephemeral bool

	// mu serializes deliveries. Notifications are rare, and serializing
	// keeps at most one refresh in flight, which Fastmail requires of a
	// rotating refresh token.
	mu     sync.Mutex
	config *emailConfig
	loaded os.FileInfo // the file config came from; nil to reload it
	// secretsReady is false while a keyring-mode config still lacks the
	// secrets from its keyring item.
	secretsReady bool
	// unsaved is a rotated refresh token not yet known to be stored.
	unsaved *rotation
	keys    *keyringClient // keyringStore, kept so late writes use the same one
	// beforeItemWrite, in tests, runs between the check that the file still
	// names this grant and the keyring write.
	beforeItemWrite func()
	access          string
	expiry          time.Time
	jmap            *jmapAccount // what the JMAP session said, cached
	recent          []time.Time  // when the sends of the last minute started
}

func (b *emailBackend) Name() string { return "email" }

// Notify runs under b.mu, so one slow delivery holds up the next. Its
// worst case is bounded, not short: reading the keyring item (at most 5 s,
// and no longer than the request's own 10 s), then a token refresh and the
// write of a rotated token, which share one uncancellable 15 s; then the
// send, on what is left of the request's context. About 20 s in all, when
// a keyring and a token endpoint both hang.
func (b *emailBackend) Notify(ctx context.Context, n Notification) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := b.reloadIfChanged(); err != nil {
		return err
	}
	if err := b.ensureSecrets(ctx); err != nil {
		return err
	}
	b.storeRotated(ctx)
	if err := b.admit(); err != nil {
		return err
	}
	message := composeEmail(n)
	switch b.config.Provider {
	case providerGoogle:
		return b.sendGmail(ctx, message)
	case providerFastmail:
		return b.sendJMAP(ctx, message)
	}
	return fmt.Errorf("unknown provider %q", b.config.Provider)
}

// admit enforces max_per_minute over a sliding minute. Every attempt
// counts, failed ones too: each may have sent an email.
func (b *emailBackend) admit() error {
	limit := b.config.perMinute()
	if limit < 0 {
		return nil
	}
	now := b.clock()
	kept := b.recent[:0]
	for _, at := range b.recent {
		if now.Sub(at) < time.Minute {
			kept = append(kept, at)
		}
	}
	b.recent = kept
	if len(b.recent) >= limit {
		wait := time.Minute - now.Sub(b.recent[0])
		return fmt.Errorf("not sent: %d emails in the last minute is the limit (max_per_minute in %s); next one possible in %s",
			limit, b.path, wait.Round(time.Second))
	}
	b.recent = append(b.recent, now)
	return nil
}

// reloadIfChanged picks up a new grant from `oshirase auth` without
// restarting the daemon.
func (b *emailBackend) reloadIfChanged() error {
	info, err := os.Stat(b.path)
	if err != nil {
		return fmt.Errorf("email configuration: %w", err)
	}
	if sameVersion(b.loaded, info) {
		return nil
	}
	config, loaded, err := loadEmailConfig(b.path)
	if err != nil {
		return err
	}
	// The same grant, in the keyring: its item can only hold what this
	// daemon wrote (auth makes a new grant id every time), and the
	// secrets in memory are at least as new, the unsaved token included. So
	// keep them rather than read the item, which may still be busy with a
	// write the daemon gave up on.
	sameGrant := config.inKeyring() && b.config.inKeyring() && config.GrantID == b.config.GrantID && b.secretsReady
	if sameGrant {
		config.setSecrets(b.config.secrets())
	}
	b.config, b.loaded, b.secretsReady = config, loaded, sameGrant || !config.inKeyring()
	b.access, b.expiry, b.jmap = "", time.Time{}, nil
	if !config.inKeyring() {
		b.reconcileRotation()
	} else if !sameGrant {
		b.unsaved = nil // a new grant; the unsaved token was the old one's
	}
	log.Printf("email: reloaded %s (provider %s)", b.path, config.Provider)
	return nil
}

// ensureSecrets reads a keyring-mode configuration's secrets from its item,
// once per load of the file; a failure is retried on the next notification.
func (b *emailBackend) ensureSecrets(ctx context.Context) error {
	if b.secretsReady {
		return nil
	}
	secrets, err := b.keys.get(ctx, b.config.GrantID)
	if err != nil {
		return keyringFailure(err, b.config)
	}
	full := *b.config
	full.setSecrets(secrets)
	if err := full.validate(); err != nil {
		return fmt.Errorf("the keyring item %s: %w; run oshirase auth %s", keyringUser(b.config.GrantID), err, b.config.Provider)
	}
	b.config, b.secretsReady = &full, true
	b.reconcileRotation()
	return nil
}

// rotation is a Fastmail refresh token that replaced others and has not
// been stored yet: its write failed, timed out, or was refused while an
// earlier one was pending. Losing it loses the grant, since the stored
// token is spent, so the daemon keeps it until a write of it succeeds.
type rotation struct {
	token string
	spent map[string]bool // the tokens it replaced since the last save
}

// noteRotation records that token replaced the current one.
func (b *emailBackend) noteRotation(token string) {
	if b.unsaved == nil {
		b.unsaved = &rotation{spent: map[string]bool{}}
	}
	b.unsaved.spent[b.config.RefreshToken] = true
	b.unsaved.token = token
	b.config.RefreshToken = token
}

// reconcileRotation runs after a load. A stored token that is one the
// unsaved token replaced (the file was only touched or edited, or a late
// write landed an older token) gives way to the unsaved one. Any other
// token is a new grant, from oshirase auth, and the unsaved token, which
// belongs to the old grant, is dropped.
func (b *emailBackend) reconcileRotation() {
	if b.unsaved == nil {
		return
	}
	switch stored := b.config.RefreshToken; {
	case stored == b.unsaved.token:
		b.unsaved = nil // it got there after all
	case b.unsaved.spent[stored]:
		b.config.RefreshToken = b.unsaved.token
	default:
		b.unsaved = nil
	}
}

// storeRotated writes the unsaved token, if there is one, where the loaded
// configuration keeps its secrets. Notify calls it first thing, so a failed
// write is retried on the next notification, and so does a keyring write
// that finishes late: whatever older token that write put in the item,
// the newest is written after it.
func (b *emailBackend) storeRotated(ctx context.Context) {
	if b.unsaved == nil || b.ephemeral {
		return
	}
	// Only into the configuration it belongs to: if the file has changed,
	// the next load decides (see reconcileRotation).
	if current, err := os.Stat(b.path); err != nil || !sameVersion(b.loaded, current) {
		return
	}
	saving := b.unsaved.token
	if b.config.inKeyring() {
		grant := b.config.GrantID
		if b.beforeItemWrite != nil {
			b.beforeItemWrite()
		}
		err := b.keys.write(ctx, grant, b.config.secrets(), func() { b.lateWrite(grant) })
		if err != nil {
			log.Printf("warning: could not save the rotated refresh token to the keyring: %v; will retry, but the grant is lost if the daemon stops first", keyringFailure(err, b.config))
			return
		}
		b.dropIfSuperseded(ctx, grant)
	} else {
		written, err := saveEmailConfig(b.path, b.config)
		if err != nil {
			log.Printf("warning: could not save the rotated refresh token to %s: %v; will retry, but the grant is lost if the daemon stops first", b.path, err)
			return
		}
		b.loaded = written
	}
	if b.unsaved != nil && b.unsaved.token == saving {
		b.unsaved = nil
	}
}

// lateWrite runs when a keyring write to grant's item, which the daemon
// gave up on, finishes after all. That write may have put an older token in
// the item than the newest one, so the newest is written again; or the
// grant may have been replaced meanwhile, and the write recreated an item
// auth had deleted.
func (b *emailBackend) lateWrite(grant string) {
	go func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		if b.dropIfSuperseded(ctx, grant) {
			return
		}
		b.storeRotated(ctx)
	}()
}

// dropIfSuperseded deletes grant's item when email.json no longer names
// it, and says whether it did. Between its check that the file is
// unchanged and its write, the daemon can recreate an item that oshirase
// auth has just replaced and deleted; auth deletes the old item only after
// writing the new file, so checking the file again after the write and
// deleting the item if it is no longer named leaves none behind, whichever
// of the two comes last. (A crash in between still can; the README says
// how to find such items.)
func (b *emailBackend) dropIfSuperseded(ctx context.Context, grant string) bool {
	named, ok := fileGrantID(b.path)
	if !ok || named == grant {
		return false
	}
	if err := b.keys.remove(ctx, grant); err != nil {
		log.Printf("warning: could not delete %s, the keyring item of a replaced grant: %v; delete it by hand (see the README)", keyringUser(grant), err)
	} else {
		log.Printf("email: deleted %s, the keyring item of a replaced grant", keyringUser(grant))
	}
	return true
}

// fileGrantID is the grant the file at path names now: "" with ok for a
// file that keeps its secrets itself; not ok when it cannot be read, in
// which case nothing should be deleted on its account.
func fileGrantID(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var named struct {
		Secrets string `json:"secrets"`
		GrantID string `json:"grant_id"`
	}
	if json.Unmarshal(data, &named) != nil {
		return "", false
	}
	if named.Secrets != secretsInKeyring {
		return "", true
	}
	return named.GrantID, true
}

// sameVersion tells whether two stats are of the same write of the file.
// Every save renames a new file into place, so a new write is a new inode;
// the modification time and size guard against a reused one.
func sameVersion(old, current os.FileInfo) bool {
	return old != nil && os.SameFile(old, current) &&
		old.ModTime().Equal(current.ModTime()) && old.Size() == current.Size()
}

func (b *emailBackend) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// accessToken returns a bearer token for the provider's API, refreshing it
// when it is about to expire.
func (b *emailBackend) accessToken(ctx context.Context) (string, error) {
	for attempt := 0; ; attempt++ {
		if b.config.APIToken != "" {
			return b.config.APIToken, nil
		}
		if b.access != "" && b.clock().Before(b.expiry.Add(-time.Minute)) {
			return b.access, nil
		}
		replaced, err := b.refresh(ctx)
		if err != nil {
			return "", err
		}
		if !replaced {
			return b.access, nil
		}
		// `oshirase auth` wrote a new grant during the refresh: load it
		// and refresh with that one instead.
		if attempt > 0 {
			return "", fmt.Errorf("%s keeps changing during token refreshes; try again", b.path)
		}
		if err := b.reloadIfChanged(); err != nil {
			return "", err
		}
		if err := b.ensureSecrets(ctx); err != nil {
			return "", err
		}
	}
}

// refresh exchanges the refresh token for an access token, and saves a
// rotated refresh token. It reports replaced, and changes nothing, when the
// file turned out to hold a different grant by the time the refresh came
// back.
func (b *emailBackend) refresh(ctx context.Context) (replaced bool, err error) {
	client := oauthClient{
		TokenEndpoint: b.config.TokenEndpoint,
		ClientID:      b.config.ClientID,
		ClientSecret:  b.config.ClientSecret,
		HTTP:          b.http,
	}
	if b.config.Provider == providerFastmail {
		// The grant was made for the JMAP API (RFC 8707); see oauthClient.
		client.Resource = b.config.JMAPSession
	}
	// The refresh and the save of a rotated token run on a context of their
	// own. The request's context ends when the caller gives up (oshirase
	// send waits 5 seconds, some callers 3), and a refresh cut off after
	// Fastmail rotated the token but before the save would lose the new
	// token, leaving the old one, whose next use revokes the grant.
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	token, err := client.refresh(refreshCtx, b.config.RefreshToken)
	if err != nil {
		return false, b.refreshFailure(err)
	}

	// Fastmail rotates the refresh token on every use and revokes the grant
	// if the old one is used again, so the new one has to be stored before
	// anything else can go wrong.
	rotated := token.RefreshToken != "" && token.RefreshToken != b.config.RefreshToken
	if rotated && b.ephemeral {
		b.config.RefreshToken = token.RefreshToken
	} else if rotated {
		// Unless `oshirase auth` replaced the file during the refresh: its
		// grant is newer than the one just rotated. Keep the file, drop the
		// old grant from memory, and let the caller load the new one.
		//
		// With the secrets in a file this check is what keeps the old grant
		// from being written over the new one; it narrows that race to the
		// moment between it and the rename. In the keyring it is not needed
		// for that: the token goes to the item of the grant this daemon
		// loaded, keyed by its id, which can never be the new grant's. It
		// switches to the new grant at once, and mostly saves recreating the
		// old item after auth deleted it; dropIfSuperseded covers the rest.
		if current, err := os.Stat(b.path); err != nil || !sameVersion(b.loaded, current) {
			log.Printf("email: %s changed during a token refresh; keeping the file's grant", b.path)
			b.loaded, b.access, b.expiry, b.jmap = nil, "", time.Time{}, nil
			return true, nil
		}
		// On refreshCtx, so the refresh and this write share its 15 s.
		b.noteRotation(token.RefreshToken)
		b.storeRotated(refreshCtx)
	}

	lifetime := time.Duration(token.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = time.Hour // unstated; a 401 later forces a refresh anyway
	}
	b.access, b.expiry = token.AccessToken, b.clock().Add(lifetime)
	return false, nil
}

// refreshFailure turns a token endpoint's refusal into what the person has
// to do about it.
func (b *emailBackend) refreshFailure(err error) error {
	provider := b.config.Provider
	var failure *oauthError
	if !errors.As(err, &failure) {
		return fmt.Errorf("%s token endpoint: %w", provider, err)
	}
	switch {
	case failure.Code == "invalid_grant" && failure.Temporary:
		return fmt.Errorf("%s refused the refresh token for now (%v); try again later", provider, failure)
	case failure.Code == "invalid_grant":
		return fmt.Errorf("refresh token revoked or expired (%v); run oshirase auth %s", failure, provider)
	case failure.Code == "invalid_target":
		return fmt.Errorf("%s does not accept %q as the API this grant is for (%v); check jmap_session in the configuration, or run oshirase auth %s", provider, b.config.JMAPSession, failure, provider)
	case failure.Code == "invalid_client" || failure.Code == "unauthorized_client":
		return fmt.Errorf("%s no longer accepts the OAuth client (%v); run oshirase auth %s", provider, failure, provider)
	default:
		return fmt.Errorf("%s token endpoint refused the refresh (%v)", provider, failure)
	}
}

// forgetAccessToken is for a 401: the token was revoked or expired early.
func (b *emailBackend) forgetAccessToken() {
	b.access, b.expiry = "", time.Time{}
}

// emailMessage is a notification as an email, independent of how it is
// sent.
type emailMessage struct {
	Subject string
	Body    string
	// Headers are extra header fields, in order: the priority ones.
	Headers [][2]string
}

// composeEmail makes the subject "[oshirase · source] title" and the body
// the notification's body (the title again when there is none, so the
// message is never empty). Line breaks in the subject become spaces.
func composeEmail(n Notification) emailMessage {
	tag := "oshirase"
	if n.Source != "" {
		tag += " · " + n.Source
	}
	subject := oneLine("[" + tag + "] " + n.Title)
	body := n.Body
	if body == "" {
		body = n.Title
	}
	return emailMessage{Subject: subject, Body: body, Headers: priorityHeaders(n.Urgency)}
}

// priorityHeaders marks critical notifications as important and low ones as
// unimportant, for the clients that show or filter on it: Importance (RFC
// 2156, Outlook) and X-Priority (most others). Normal adds nothing.
func priorityHeaders(urgency Urgency) [][2]string {
	switch urgency {
	case UrgencyCritical:
		return [][2]string{{"Importance", "high"}, {"X-Priority", "1 (Highest)"}}
	case UrgencyLow:
		return [][2]string{{"Importance", "low"}, {"X-Priority", "5 (Lowest)"}}
	}
	return nil
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
