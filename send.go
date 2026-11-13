package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// From a Lima guest, host.lima.internal is the host's own loopback, which is
// where the daemon listens by default.
const defaultURL = "http://host.lima.internal:4649"

func runSend(args []string) error {
	flags := flag.NewFlagSet("send", flag.ContinueOnError)
	baseURL := flags.String("url", envOr("OSHIRASE_URL", defaultURL), "daemon address")
	urgency := flags.String("urgency", "normal", "low, normal or critical")
	source := flags.String("source", "", "what is sending this, e.g. ssh")
	tag := flags.String("tag", "", "a tag for `oshirase withdraw` to take it down by, e.g. ssh-touch")
	via := flags.String("via", "", `"email" to send this one as an email, set up on the host with oshirase auth; empty for the daemon's own backend`)
	timeout := flags.Duration("timeout", 5*time.Second, "give up after this long")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: oshirase send [flags] <title> [body]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}

	positional := flags.Args()
	if len(positional) < 1 || len(positional) > 2 {
		flags.Usage()
		return errUsage
	}

	notification := Notification{
		Title:   positional[0],
		Urgency: Urgency(*urgency),
		Source:  *source,
		Tag:     *tag,
		Via:     *via,
	}
	if len(positional) == 2 {
		notification.Body = positional[1]
	}
	// Check here too, so a bad flag fails before anything goes over the wire.
	if err := notification.Normalize(); err != nil {
		return err
	}
	if notification.Via == ViaEmail && *timeout < emailSendTimeout {
		// An email takes a token refresh and a send, which can outlast
		// the default; giving up early would not stop the email going out.
		*timeout = emailSendTimeout
	}
	body, err := post(*baseURL, "/v1/notify", notification, *timeout)
	if err != nil {
		return err
	}
	return checkDeliveredVia(notification.Via, body)
}

// emailSendTimeout is the least time send waits for an email. Its worst
// case is about 25 s: opening the email backend on first use (a keyring
// read, 5 s), the keyring again in Notify (5 s), a token refresh and the
// write of a rotated token (15 s, not cut short by notifyTimeout), then
// the send.
const emailSendTimeout = 30 * time.Second

// checkDeliveredVia catches a daemon older than Via, which ignores the
// field and shows the notification on the screen instead.
func checkDeliveredVia(via string, body []byte) error {
	if via == "" {
		return nil
	}
	var response NotifyResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return fmt.Errorf("the daemon's answer is not JSON: %w", err)
	}
	if response.Backend != via {
		return fmt.Errorf("the daemon delivered it with its %s backend, not via %s; it is too old for -via (needs v0.5.0)", response.Backend, via)
	}
	return nil
}

func runWithdraw(args []string) error {
	flags := flag.NewFlagSet("withdraw", flag.ContinueOnError)
	baseURL := flags.String("url", envOr("OSHIRASE_URL", defaultURL), "daemon address")
	tag := flags.String("tag", "", "take down the notifications sent with this tag (required)")
	timeout := flags.Duration("timeout", 5*time.Second, "give up after this long")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: oshirase withdraw -tag <tag> [flags]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *tag == "" {
		flags.Usage()
		return errUsage
	}
	if err := NormalizeTag(*tag); err != nil {
		return err
	}
	_, err := post(*baseURL, "/v1/withdraw", WithdrawRequest{Tag: *tag}, *timeout)
	return err
}

// post sends a JSON request to the daemon and returns the body of a 200.
func post(baseURL, path string, request any, timeout time.Duration) ([]byte, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: timeout}
	endpoint := strings.TrimRight(baseURL, "/") + path
	response, err := client.Post(endpoint, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("no daemon answered at %s: %w", baseURL, err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxRequestSize))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		var failure ErrorResponse
		if json.Unmarshal(responseBody, &failure) == nil && failure.Error != "" {
			return nil, fmt.Errorf("daemon refused (%s): %s", response.Status, failure.Error)
		}
		return nil, fmt.Errorf("daemon refused (%s)", response.Status)
	}
	return responseBody, nil
}

var errUsage = errors.New("usage")
