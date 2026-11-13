package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"
)

// `oshirase auth google|fastmail` makes the grant the email backend uses,
// once, interactively, and stores it: the secrets in the OS keyring (or,
// with -store file, in email.json), the rest in email.json.

const authUsage = `usage:
  oshirase auth google -credentials client_secret.json -to you@example.com [-from addr]
  oshirase auth google -client-id ID -client-secret SECRET -to you@example.com [-from addr]
  oshirase auth fastmail -to you@example.com [-from addr]
  oshirase auth fastmail -api-token -to you@example.com [-from addr] < token

Every form takes -store keyring (the default) or -store file, and -config path.
`

// authKeyringTimeout replaces the daemon's 5 seconds: a person is at the
// terminal, and the keyring may ask them to unlock it.
const authKeyringTimeout = 2 * time.Minute

// softwareID identifies oshirase to authorization servers that register
// clients dynamically; it stays the same across versions (RFC 7591 §2).
const softwareID = "6f1c2a8e-4b7d-4f0e-9a51-0b8d3c6e2f47"

func runAuth(args []string) error {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, authUsage)
		return errUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	keyringStore.timeout = authKeyringTimeout

	switch args[0] {
	case providerGoogle:
		return runAuthGoogle(ctx, args[1:])
	case providerFastmail:
		return runAuthFastmail(ctx, args[1:])
	default:
		fmt.Fprint(os.Stderr, authUsage)
		return errUsage
	}
}

// presentURL prints the authorization URL for the person to open. It is
// not opened for them: the browser that holds their session may be on
// another machine than the one this runs on.
func presentURL(authURL string) {
	fmt.Fprintf(os.Stderr, "Open this URL in a browser on this machine and allow access:\n\n  %s\n\nWaiting for the redirect (Ctrl-C to give up)...\n", authURL)
}

// authFlags are the flags every form of auth takes.
type authFlags struct {
	to, from, configPath, store *string
}

func addAuthFlags(flags *flag.FlagSet) authFlags {
	return authFlags{
		to:         flags.String("to", "", "address the notifications go to (required)"),
		from:       flags.String("from", "", "address to send as (default: the account's own)"),
		configPath: flags.String("config", "", "configuration file (default $OSHIRASE_EMAIL_CONFIG, else email.json in the user config directory)"),
		store:      flags.String("store", secretsInKeyring, "where the secrets go: keyring (the OS keyring), or file (email.json, mode 0600) where there is no keyring"),
	}
}

// check validates the flags and resolves the configuration path.
func (f authFlags) check() (path string, err error) {
	if *f.store != secretsInKeyring && *f.store != secretsInFile {
		return "", fmt.Errorf("-store must be keyring or file, not %q", *f.store)
	}
	if err := checkAddresses(*f.to, *f.from); err != nil {
		return "", err
	}
	return resolveConfigPath(*f.configPath)
}

func checkAddresses(to, from string) error {
	if to == "" {
		return errors.New("-to is required")
	}
	if _, err := mail.ParseAddress(to); err != nil {
		return fmt.Errorf("-to: %w", err)
	}
	if from != "" {
		if _, err := mail.ParseAddress(from); err != nil {
			return fmt.Errorf("-from: %w", err)
		}
	}
	return nil
}

func resolveConfigPath(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	return emailConfigPath()
}

func runAuthGoogle(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("auth google", flag.ContinueOnError)
	credentials := flags.String("credentials", "", "the client JSON downloaded from Google Cloud (Desktop app client)")
	clientID := flags.String("client-id", "", "OAuth client id, if not using -credentials")
	clientSecret := flags.String("client-secret", "", "OAuth client secret, if not using -credentials (shows in ps and shell history; prefer -credentials)")
	common := addAuthFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	path, err := common.check()
	if err != nil {
		return err
	}

	client := googleClient{ID: *clientID, Secret: *clientSecret, TokenEndpoint: googleTokenEndpoint}
	if *credentials != "" {
		if client, err = readGoogleCredentials(*credentials); err != nil {
			return err
		}
	}
	if client.ID == "" || client.Secret == "" {
		return errors.New("give -credentials, or both -client-id and -client-secret")
	}

	config, err := authGoogle(ctx, client, googleAuthEndpoint, presentURL, http.DefaultClient)
	if err != nil {
		return err
	}
	config.To, config.From = *common.to, *common.from
	if err := storeGrant(ctx, path, config, *common.store, os.Stderr); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Saved the grant: %s.\nStart the daemon with: oshirase serve -backend email\n", describeStore(path, config))
	return nil
}

type googleClient struct {
	ID, Secret, TokenEndpoint string
}

// readGoogleCredentials reads the JSON Google Cloud offers for download
// next to a Desktop app client: {"installed": {"client_id": ..., ...}}.
func readGoogleCredentials(path string) (googleClient, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return googleClient{}, err
	}
	var file struct {
		Installed *struct {
			ClientID     string `json:"client_id"`
			ClientSecret string `json:"client_secret"`
			TokenURI     string `json:"token_uri"`
		} `json:"installed"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return googleClient{}, fmt.Errorf("%s: %w", path, err)
	}
	if file.Installed == nil {
		return googleClient{}, fmt.Errorf("%s is not a Desktop app client (no \"installed\" key); create the client with application type Desktop app", path)
	}
	client := googleClient{
		ID:            file.Installed.ClientID,
		Secret:        file.Installed.ClientSecret,
		TokenEndpoint: file.Installed.TokenURI,
	}
	if client.TokenEndpoint == "" {
		client.TokenEndpoint = googleTokenEndpoint
	}
	return client, nil
}

func authGoogle(ctx context.Context, client googleClient, authEndpoint string, present func(string), httpc *http.Client) (*emailConfig, error) {
	grant, err := authorizeLoopback(ctx, authorizeRequest{
		Endpoint: authEndpoint,
		Params: url.Values{
			"client_id": {client.ID},
			"scope":     {gmailSendScope},
			// A refresh token, and a new one even if the account has
			// granted this client before.
			"access_type": {"offline"},
			"prompt":      {"consent"},
		},
		Present: present,
	})
	if err != nil {
		return nil, err
	}

	oauth := oauthClient{TokenEndpoint: client.TokenEndpoint, ClientID: client.ID, ClientSecret: client.Secret, HTTP: httpc}
	token, err := oauth.exchange(ctx, grant.Code, grant.Verifier, grant.RedirectURI)
	if err != nil {
		return nil, fmt.Errorf("exchanging the code: %w", err)
	}
	if token.RefreshToken == "" {
		return nil, errors.New("google granted no refresh token; revoke oshirase at myaccount.google.com/permissions and run this again")
	}
	if token.Scope != "" && !slices.Contains(strings.Fields(token.Scope), gmailSendScope) {
		return nil, fmt.Errorf("the grant does not include %s; allow sending email on the consent screen", gmailSendScope)
	}
	return &emailConfig{
		Provider:      providerGoogle,
		TokenEndpoint: client.TokenEndpoint,
		ClientID:      client.ID,
		ClientSecret:  client.Secret,
		RefreshToken:  token.RefreshToken,
	}, nil
}

func runAuthFastmail(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("auth fastmail", flag.ContinueOnError)
	apiToken := flags.Bool("api-token", false, "read an API token from standard input instead of making an OAuth grant")
	issuer := flags.String("issuer", fastmailIssuer, "OAuth authorization server")
	session := flags.String("session", fastmailJMAPSession, "JMAP session URL")
	common := addAuthFlags(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	path, err := common.check()
	if err != nil {
		return err
	}

	var config *emailConfig
	var access *tokenResponse
	if *apiToken {
		token, err := readAPIToken(os.Stdin)
		if err != nil {
			return err
		}
		config = &emailConfig{Provider: providerFastmail, APIToken: token, JMAPSession: *session}
	} else {
		config, access, err = authFastmail(ctx, *issuer, *session, presentURL, http.DefaultClient)
		if err != nil {
			return err
		}
	}
	config.To, config.From = *common.to, *common.from

	// Find the sending identity now, so a grant that cannot send fails here
	// rather than at the first notification.
	if err := resolveFastmailIdentity(ctx, path, config, access, http.DefaultClient); err != nil {
		return err
	}
	if err := storeGrant(ctx, path, config, *common.store, os.Stderr); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Saved the grant: %s. Notifications will be sent as %s to %s.\nStart the daemon with: oshirase serve -backend email\n", describeStore(path, config), config.From, config.To)
	return nil
}

// storeGrant saves a new grant. In the keyring, it writes the new grant's
// item first, then email.json, so the file never names an item that does
// not exist; then it deletes the previous grant's item, best effort (a
// failure is reported, not fatal: the new grant is in place). A grant for
// -store file goes into email.json whole, and the previous grant's item, if
// there was one, is deleted as well, so moving between the two leaves
// nothing behind.
func storeGrant(ctx context.Context, path string, config *emailConfig, store string, report io.Writer) error {
	previous := previousGrantID(path)

	switch store {
	case secretsInKeyring:
		config.Secrets, config.GrantID = secretsInKeyring, newGrantID()
		item := keyringUser(config.GrantID)
		if err := keyringStore.set(ctx, config.GrantID, config.secrets()); err != nil {
			if errors.Is(err, errKeyringTimeout) || errors.Is(err, context.Canceled) {
				// A write that timed out or was interrupted may still land.
				fmt.Fprintf(report, "note: if the keyring item %s appears later, nothing uses it; delete it (see the README, \"Leftover keyring items\")\n", item)
			}
			return fmt.Errorf("storing the grant: %w", keyringFailure(err, config))
		}
		if _, err := saveEmailConfig(path, config); err != nil {
			// Undo the item, on a context of its own: ctx may be what ended.
			undo, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if rmErr := keyringStore.remove(undo, config.GrantID); rmErr != nil {
				fmt.Fprintf(report, "warning: could not delete the keyring item %s after failing to write %s: %v; nothing uses it, delete it by hand (see the README, \"Leftover keyring items\")\n", item, path, rmErr)
			}
			return err
		}
	case secretsInFile:
		config.Secrets, config.GrantID = secretsInFile, ""
		if _, err := saveEmailConfig(path, config); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown store %q", store)
	}

	if previous != "" && previous != config.GrantID {
		if err := keyringStore.remove(ctx, previous); err != nil {
			fmt.Fprintf(report, "warning: could not delete the previous grant's keyring item %s: %v; nothing uses it now, delete it by hand (see the README, \"Leftover keyring items\")\n", keyringUser(previous), err)
		}
	}
	return nil
}

// previousGrantID is the grant id of the configuration at path, if it is in
// keyring mode; anything else (no file, a plaintext one, one that does not
// parse) has no item to delete.
func previousGrantID(path string) string {
	grant, _ := fileGrantID(path)
	return grant
}

func describeStore(path string, config *emailConfig) string {
	if config.inKeyring() {
		return fmt.Sprintf("secrets in %s (item %s), the rest in %s", keyringName(), keyringUser(config.GrantID), path)
	}
	return "everything in " + path + " (mode 0600)"
}

func readAPIToken(input io.Reader) (string, error) {
	if file, ok := input.(*os.File); ok {
		if info, err := file.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			fmt.Fprint(os.Stderr, "Paste the API token (it will show as you type) and press Enter: ")
		}
	}
	line, err := bufio.NewReader(input).ReadString('\n')
	token := strings.TrimSpace(line)
	if token == "" {
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return "", errors.New("no API token on standard input")
	}
	return token, nil
}

// resolveFastmailIdentity reads the JMAP session with the new credential,
// checks it may send, and fills in From when it was not given. It uses the
// backend itself, with the access token the grant just returned (nil for an
// API token), so the new refresh token is normally not spent here. If it is
// (the access token came back already near expiry), the rotated token
// lands in config and nowhere else: the file at path is the previous
// grant's until the caller saves the complete configuration.
func resolveFastmailIdentity(ctx context.Context, path string, config *emailConfig, access *tokenResponse, httpc *http.Client) error {
	probe := &emailBackend{path: path, http: httpc, config: config, ephemeral: true, keys: keyringStore}
	if access != nil {
		lifetime := time.Duration(access.ExpiresIn) * time.Second
		if lifetime <= 0 {
			lifetime = time.Hour // unstated, as in accessToken
		}
		probe.access = access.AccessToken
		probe.expiry = time.Now().Add(lifetime)
	}
	account, err := probe.jmapPrepare(ctx)
	if err != nil {
		return fmt.Errorf("checking the grant against JMAP: %w", err)
	}
	config.From = account.From
	return nil
}

// fastmailMetadata is the part of RFC 8414 authorization server metadata
// that the flow needs.
type fastmailMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ScopesSupported       []string `json:"scopes_supported"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
	IssuerInResponse      bool     `json:"authorization_response_iss_parameter_supported"`
}

// authFastmail follows the OAuth Profile for Open Public Clients
// (draft-ietf-mailmaint-oauth-public), which Fastmail implements: discover
// the server's metadata, register a public client dynamically (RFC 7591)
// with a loopback redirect, then the authorization-code grant with PKCE.
// No client has to be registered with Fastmail by hand.
func authFastmail(ctx context.Context, issuer, session string, present func(string), httpc *http.Client) (*emailConfig, *tokenResponse, error) {
	metadata, err := discoverFastmail(ctx, issuer, httpc)
	if err != nil {
		return nil, nil, err
	}

	// The mail scope covers JMAP mail and submission; offline_access is to
	// be asked for whenever the server lists it.
	scopes := []string{"urn:ietf:params:oauth:scope:mail"}
	if slices.Contains(metadata.ScopesSupported, "offline_access") {
		scopes = append(scopes, "offline_access")
	}
	scope := strings.Join(scopes, " ")

	clientID, err := registerClient(ctx, metadata.RegistrationEndpoint, scope, httpc)
	if err != nil {
		return nil, nil, err
	}

	grant, err := authorizeLoopback(ctx, authorizeRequest{
		Endpoint: metadata.AuthorizationEndpoint,
		Params: url.Values{
			"client_id": {clientID},
			"scope":     {scope},
			"prompt":    {"consent"},
			// Fastmail binds each grant to the API it is for (RFC 8707)
			// and answers invalid_target without one. The JMAP session
			// URL is that API's resource identifier, as its 401 says
			// (WWW-Authenticate: Bearer resource_metadata=...).
			"resource": {session},
		},
		Issuer:        metadata.Issuer,
		RequireIssuer: metadata.IssuerInResponse,
		Present:       present,
	})
	if err != nil {
		return nil, nil, err
	}

	oauth := oauthClient{TokenEndpoint: metadata.TokenEndpoint, ClientID: clientID, Resource: session, HTTP: httpc}
	token, err := oauth.exchange(ctx, grant.Code, grant.Verifier, grant.RedirectURI)
	if err != nil {
		return nil, nil, fmt.Errorf("exchanging the code: %w", err)
	}
	if token.RefreshToken == "" {
		return nil, nil, errors.New("fastmail granted no refresh token")
	}
	return &emailConfig{
		Provider:      providerFastmail,
		TokenEndpoint: metadata.TokenEndpoint,
		ClientID:      clientID,
		RefreshToken:  token.RefreshToken,
		JMAPSession:   session,
	}, token, nil
}

func discoverFastmail(ctx context.Context, issuer string, httpc *http.Client) (*fastmailMetadata, error) {
	issuer = strings.TrimRight(issuer, "/")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/oauth-authorization-server", nil)
	if err != nil {
		return nil, err
	}
	response, err := httpClient(httpc).Do(request)
	if err != nil {
		return nil, fmt.Errorf("discovering the authorization server: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("discovering the authorization server: %s", response.Status)
	}
	var metadata fastmailMetadata
	if err := json.NewDecoder(io.LimitReader(response.Body, maxOAuthResponse)).Decode(&metadata); err != nil {
		return nil, fmt.Errorf("discovering the authorization server: %w", err)
	}
	switch {
	case metadata.Issuer != issuer:
		return nil, fmt.Errorf("the metadata names issuer %q, not %q", metadata.Issuer, issuer)
	case metadata.RegistrationEndpoint == "":
		return nil, errors.New("the server does not offer dynamic client registration; use -api-token")
	case metadata.AuthorizationEndpoint == "" || metadata.TokenEndpoint == "":
		return nil, errors.New("the server metadata lacks an authorization or token endpoint")
	case !slices.Contains(metadata.CodeChallengeMethods, "S256"):
		return nil, errors.New("the server does not support PKCE with S256")
	}
	return &metadata, nil
}

// registerClient registers oshirase as a public native client. The
// redirect URI is registered without a port; the profile lets each
// authorization add one.
func registerClient(ctx context.Context, endpoint, scope string, httpc *http.Client) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"client_name":                "oshirase",
		"client_uri":                 "https://github.com/moroz/oshirase",
		"redirect_uris":              []string{"http://127.0.0.1/"},
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"scope":                      scope,
		"software_id":                softwareID,
		"software_version":           version,
	})
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := httpClient(httpc).Do(request)
	if err != nil {
		return "", fmt.Errorf("registering the client: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxOAuthResponse))
	if err != nil {
		return "", fmt.Errorf("registering the client: %w", err)
	}
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		failure := &oauthError{Status: response.StatusCode}
		_ = json.Unmarshal(body, failure)
		return "", fmt.Errorf("registering the client: %w", failure)
	}
	var registered struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(body, &registered); err != nil || registered.ClientID == "" {
		return "", errors.New("registering the client: the response carried no client_id")
	}
	return registered.ClientID, nil
}
