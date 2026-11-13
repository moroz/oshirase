package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"strings"
	"time"
)

// Gmail: the API's users.messages.send with the gmail.send scope, which can
// send as the account and nothing else (it cannot read mail).
const (
	googleAuthEndpoint  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenEndpoint = "https://oauth2.googleapis.com/token"
	gmailSendScope      = "https://www.googleapis.com/auth/gmail.send"
	gmailSendURL        = "https://gmail.googleapis.com/gmail/v1/users/me/messages/send"
)

func (b *emailBackend) sendGmail(ctx context.Context, message emailMessage) error {
	raw := buildRFC5322(b.config.From, b.config.To, message, b.clock())
	payload, err := json.Marshal(map[string]string{
		"raw": base64.URLEncoding.EncodeToString(raw),
	})
	if err != nil {
		return err
	}

	// One retry, after a 401: the access token was revoked or expired early.
	for attempt := 0; ; attempt++ {
		token, err := b.accessToken(ctx)
		if err != nil {
			return err
		}
		status, detail, err := b.postGmail(ctx, token, payload)
		if err != nil {
			return fmt.Errorf("gmail: %w", err)
		}
		switch {
		case status >= 200 && status < 300:
			return nil
		case status == http.StatusUnauthorized && attempt == 0:
			b.forgetAccessToken()
			continue
		case status == http.StatusUnauthorized:
			return fmt.Errorf("gmail rejected a fresh access token (%s); run oshirase auth google", detail)
		default:
			return fmt.Errorf("gmail refused the message (%s)", detail)
		}
	}
}

// postGmail returns the status and, for a failure, Google's own account of
// it ("403 PERMISSION_DENIED: ..."); never the request.
func (b *emailBackend) postGmail(ctx context.Context, token string, payload []byte) (int, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, b.gmailSendURL, bytes.NewReader(payload))
	if err != nil {
		return 0, "", err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")

	response, err := httpClient(b.http).Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxOAuthResponse))
	if err != nil {
		return 0, "", err
	}
	if response.StatusCode < 300 {
		return response.StatusCode, "", nil
	}

	var failure struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	detail := response.Status
	if json.Unmarshal(body, &failure) == nil && failure.Error.Message != "" {
		detail = fmt.Sprintf("%d %s: %s", response.StatusCode, failure.Error.Status, truncate(failure.Error.Message, 200))
	}
	return response.StatusCode, detail, nil
}

// buildRFC5322 writes a plain-text UTF-8 message. Gmail fills in From (as
// the authorized account) when it is empty, and Message-ID.
func buildRFC5322(from, to string, message emailMessage, now time.Time) []byte {
	var out bytes.Buffer
	header := func(name, value string) {
		out.WriteString(name + ": " + value + "\r\n")
	}
	if from != "" {
		header("From", (&mail.Address{Name: "oshirase", Address: from}).String())
	}
	header("To", (&mail.Address{Address: to}).String())
	// Q-encoding also encodes any control character, so nothing in the
	// title can start a header of its own.
	header("Subject", mime.QEncoding.Encode("utf-8", message.Subject))
	header("Date", now.Format(time.RFC1123Z))
	header("MIME-Version", "1.0")
	header("Content-Type", "text/plain; charset=utf-8")
	header("Content-Transfer-Encoding", "quoted-printable")
	for _, field := range message.Headers {
		header(field[0], field[1])
	}
	out.WriteString("\r\n")

	writer := quotedprintable.NewWriter(&out)
	body := strings.ReplaceAll(strings.ReplaceAll(message.Body, "\r\n", "\n"), "\n", "\r\n")
	writer.Write([]byte(body))
	writer.Close()
	return out.Bytes()
}
