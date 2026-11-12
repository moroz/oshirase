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
	}
	if len(positional) == 2 {
		notification.Body = positional[1]
	}
	// Check here too, so a bad flag fails before anything goes over the wire.
	if err := notification.Normalize(); err != nil {
		return err
	}

	payload, err := json.Marshal(notification)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: *timeout}
	endpoint := strings.TrimRight(*baseURL, "/") + "/v1/notify"
	response, err := client.Post(endpoint, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("no daemon answered at %s: %w", *baseURL, err)
	}
	defer response.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxRequestSize))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		var failure ErrorResponse
		if json.Unmarshal(responseBody, &failure) == nil && failure.Error != "" {
			return fmt.Errorf("daemon refused (%s): %s", response.Status, failure.Error)
		}
		return fmt.Errorf("daemon refused (%s)", response.Status)
	}
	return nil
}

var errUsage = errors.New("usage")
