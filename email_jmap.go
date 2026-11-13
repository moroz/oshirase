package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Fastmail: JMAP (RFC 8620, RFC 8621). Sending is two method calls in one
// request: Email/set creates the message in Drafts, EmailSubmission/set
// sends it and, on success, moves it to Sent, as Fastmail's own samples do.

const (
	fastmailIssuer      = "https://api.fastmail.com"
	fastmailJMAPSession = "https://api.fastmail.com/jmap/session"

	jmapCore       = "urn:ietf:params:jmap:core"
	jmapMail       = "urn:ietf:params:jmap:mail"
	jmapSubmission = "urn:ietf:params:jmap:submission"
)

// jmapAccount is what sending needs from the session, cached between
// notifications and dropped after any failure.
type jmapAccount struct {
	APIURL     string
	AccountID  string
	IdentityID string
	From       string
	Drafts     string // mailbox ids; Sent may be empty
	Sent       string
}

func (b *emailBackend) sendJMAP(ctx context.Context, message emailMessage) error {
	account, err := b.jmapPrepare(ctx)
	if err != nil {
		return err
	}

	draft := map[string]any{
		"mailboxIds": map[string]bool{account.Drafts: true},
		"keywords":   map[string]bool{"$draft": true, "$seen": true},
		"from":       []map[string]string{{"name": "oshirase", "email": account.From}},
		"to":         []map[string]string{{"email": b.config.To}},
		"subject":    message.Subject,
		"bodyValues": map[string]any{"body": map[string]string{"value": message.Body}},
		"textBody":   []map[string]string{{"partId": "body", "type": "text/plain"}},
	}
	for _, field := range message.Headers {
		draft["header:"+field[0]+":asText"] = field[1]
	}

	submit := map[string]any{
		"accountId": account.AccountID,
		"create": map[string]any{
			"send": map[string]string{"identityId": account.IdentityID, "emailId": "#draft"},
		},
	}
	if account.Sent != "" {
		submit["onSuccessUpdateEmail"] = map[string]any{
			"#send": map[string]any{
				"mailboxIds/" + account.Drafts: nil,
				"mailboxIds/" + account.Sent:   true,
				"keywords/$draft":              nil,
			},
		}
	} else {
		submit["onSuccessDestroyEmail"] = []string{"#send"}
	}

	responses, err := b.jmapCall(ctx, account.APIURL, []any{
		[]any{"Email/set", map[string]any{"accountId": account.AccountID, "create": map[string]any{"draft": draft}}, "0"},
		[]any{"EmailSubmission/set", submit, "1"},
	})
	if err == nil {
		err = firstNotCreated(responses, "Email/set", "EmailSubmission/set")
	}
	if err != nil {
		b.jmap = nil // the identity or a mailbox may have changed; look again next time
		return err
	}
	return nil
}

// jmapPrepare finds the account, the identity that sends as From, and the
// Drafts and Sent mailboxes.
func (b *emailBackend) jmapPrepare(ctx context.Context) (*jmapAccount, error) {
	if b.jmap != nil {
		return b.jmap, nil
	}

	var session struct {
		APIURL          string                     `json:"apiUrl"`
		Capabilities    map[string]json.RawMessage `json:"capabilities"`
		PrimaryAccounts map[string]string          `json:"primaryAccounts"`
	}
	if err := b.jmapRequest(ctx, http.MethodGet, b.config.JMAPSession, nil, &session); err != nil {
		return nil, err
	}
	if _, ok := session.Capabilities[jmapSubmission]; !ok {
		return nil, errors.New("the JMAP session offers no submission capability; the grant does not allow sending mail")
	}
	accountID := session.PrimaryAccounts[jmapSubmission]
	if accountID == "" || session.APIURL == "" {
		return nil, errors.New("the JMAP session names no account to send from")
	}

	responses, err := b.jmapCall(ctx, session.APIURL, []any{
		[]any{"Identity/get", map[string]any{"accountId": accountID}, "0"},
		[]any{"Mailbox/query", map[string]any{"accountId": accountID, "filter": map[string]string{"role": "drafts"}}, "1"},
		[]any{"Mailbox/query", map[string]any{"accountId": accountID, "filter": map[string]string{"role": "sent"}}, "2"},
	})
	if err != nil {
		return nil, err
	}

	var identities struct {
		List []struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"list"`
	}
	var drafts, sent struct {
		IDs []string `json:"ids"`
	}
	if err := responses.decode("0", &identities); err != nil {
		return nil, err
	}
	if err := responses.decode("1", &drafts); err != nil {
		return nil, err
	}
	if err := responses.decode("2", &sent); err != nil {
		return nil, err
	}

	account := &jmapAccount{APIURL: session.APIURL, AccountID: accountID}
	var addresses []string
	for _, identity := range identities.List {
		addresses = append(addresses, identity.Email)
		if b.config.From == "" || strings.EqualFold(identity.Email, b.config.From) {
			account.IdentityID, account.From = identity.ID, identity.Email
			break
		}
	}
	if account.IdentityID == "" {
		if len(addresses) == 0 {
			return nil, errors.New("the account has no sending identity")
		}
		return nil, fmt.Errorf("no sending identity for %s; the account has %s", b.config.From, strings.Join(addresses, ", "))
	}
	if len(drafts.IDs) == 0 {
		return nil, errors.New("the account has no Drafts mailbox to create the message in")
	}
	account.Drafts = drafts.IDs[0]
	if len(sent.IDs) > 0 {
		account.Sent = sent.IDs[0]
	}
	b.jmap = account
	return account, nil
}

// jmapResponses maps call ids to [name, arguments].
type jmapResponses map[string]jmapResponse

type jmapResponse struct {
	Name      string
	Arguments json.RawMessage
}

func (r jmapResponses) decode(callID string, into any) error {
	response, ok := r[callID]
	if !ok {
		return fmt.Errorf("JMAP response is missing call %s", callID)
	}
	return json.Unmarshal(response.Arguments, into)
}

// jmapCall makes one API request and fails on the first method-level error.
func (b *emailBackend) jmapCall(ctx context.Context, apiURL string, calls []any) (jmapResponses, error) {
	request := map[string]any{
		"using":       []string{jmapCore, jmapMail, jmapSubmission},
		"methodCalls": calls,
	}
	var envelope struct {
		MethodResponses [][3]json.RawMessage `json:"methodResponses"`
	}
	if err := b.jmapRequest(ctx, http.MethodPost, apiURL, request, &envelope); err != nil {
		return nil, err
	}

	responses := jmapResponses{}
	for _, triple := range envelope.MethodResponses {
		var name, callID string
		if json.Unmarshal(triple[0], &name) != nil || json.Unmarshal(triple[2], &callID) != nil {
			return nil, errors.New("JMAP response is malformed")
		}
		if name == "error" {
			var failure struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			}
			json.Unmarshal(triple[1], &failure)
			return nil, fmt.Errorf("JMAP refused the request (%s)", describe(failure.Type, failure.Description))
		}
		responses[callID] = jmapResponse{Name: name, Arguments: triple[1]}
	}
	return responses, nil
}

// firstNotCreated reports a /set call that did not create what it was
// asked to. methods are the calls in order, with call ids "0", "1", ...
// A message created but not submitted stays in Drafts.
func firstNotCreated(responses jmapResponses, methods ...string) error {
	for index, method := range methods {
		callID := fmt.Sprint(index)
		var result struct {
			NotCreated map[string]struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			} `json:"notCreated"`
		}
		if err := responses.decode(callID, &result); err != nil {
			return err
		}
		for _, failure := range result.NotCreated {
			return fmt.Errorf("%s failed (%s)", method, describe(failure.Type, failure.Description))
		}
	}
	return nil
}

// jmapRequest sends one authorized request, retrying once after a 401 with
// a fresh access token.
func (b *emailBackend) jmapRequest(ctx context.Context, method, target string, payload, into any) error {
	var body []byte
	if payload != nil {
		var err error
		if body, err = json.Marshal(payload); err != nil {
			return err
		}
	}

	for attempt := 0; ; attempt++ {
		token, err := b.accessToken(ctx)
		if err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Accept", "application/json")
		if payload != nil {
			request.Header.Set("Content-Type", "application/json")
		}

		response, err := httpClient(b.http).Do(request)
		if err != nil {
			return fmt.Errorf("fastmail: %w", err)
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, maxOAuthResponse))
		response.Body.Close()
		if err != nil {
			return fmt.Errorf("fastmail: %w", err)
		}

		switch {
		case response.StatusCode == http.StatusUnauthorized && b.config.APIToken != "":
			return errors.New("fastmail rejected the API token; make a new one and run oshirase auth fastmail -api-token")
		case response.StatusCode == http.StatusUnauthorized && attempt == 0:
			b.forgetAccessToken()
			continue
		case response.StatusCode == http.StatusUnauthorized:
			return errors.New("fastmail rejected a fresh access token; run oshirase auth fastmail")
		case response.StatusCode >= 300:
			// RFC 7807 problem details, which JMAP uses for request-level
			// errors.
			var problem struct {
				Type   string `json:"type"`
				Detail string `json:"detail"`
			}
			json.Unmarshal(data, &problem)
			detail := describe(problem.Type, problem.Detail)
			if detail == "" {
				detail = response.Status
			}
			return fmt.Errorf("fastmail refused the request (%s)", detail)
		}
		if err := json.Unmarshal(data, into); err != nil {
			return fmt.Errorf("fastmail answered with something other than JMAP: %w", err)
		}
		return nil
	}
}

func describe(kind, description string) string {
	switch {
	case kind != "" && description != "":
		return kind + ": " + truncate(description, 200)
	case kind != "":
		return kind
	default:
		return truncate(description, 200)
	}
}
