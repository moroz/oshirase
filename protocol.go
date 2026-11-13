package main

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ProtocolVersion is the version in the URL path (/v1/...). Fields may be
// added to a version; none are removed or change meaning within it.
const ProtocolVersion = 1

const (
	maxTitleRunes  = 256
	maxBodyRunes   = 4096
	maxSourceRunes = 64
	maxTagRunes    = 64
	maxRequestSize = 64 << 10
)

// NormalizeTag checks a tag on its own, for POST /v1/withdraw.
func NormalizeTag(tag string) error {
	if tag == "" {
		return errors.New("tag is required")
	}
	return (&Notification{Title: "-", Tag: tag}).Normalize()
}

type Urgency string

const (
	UrgencyLow      Urgency = "low"
	UrgencyNormal   Urgency = "normal"
	UrgencyCritical Urgency = "critical"
)

// Notification is the body of POST /v1/notify.
type Notification struct {
	// Title is the only required field.
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	// Urgency is low, normal or critical; empty means normal.
	Urgency Urgency `json:"urgency,omitempty"`
	// Source names what sent it ("ssh", "claude-code"); shown where the
	// platform has room for it.
	Source string `json:"source,omitempty"`
	// Tag groups notifications so POST /v1/withdraw can take them down
	// together once they are out of date, e.g. "ssh-touch" when the key has
	// been touched.
	Tag string `json:"tag,omitempty"`
	// Via picks how this one notification is delivered: empty for the
	// daemon's own backend (the screen, normally), or ViaEmail for an
	// email. A daemon older than the field ignores it and shows the
	// notification as usual, so a client asking for email checks the
	// backend named in the response.
	Via string `json:"via,omitempty"`
}

// ViaEmail is the one value of Notification.Via so far.
const ViaEmail = "email"

// WithdrawRequest is the body of POST /v1/withdraw.
type WithdrawRequest struct {
	Tag string `json:"tag"`
}

// WithdrawResponse is the body of a successful POST /v1/withdraw: how many
// notifications with the tag the backend asked to be closed. Some may have
// been dismissed or have expired already; the platform does not say.
type WithdrawResponse struct {
	Withdrawn int    `json:"withdrawn"`
	Backend   string `json:"backend"`
}

// NotifyResponse is the body of a successful POST /v1/notify.
type NotifyResponse struct {
	Delivered bool   `json:"delivered"`
	Backend   string `json:"backend"`
}

// HealthResponse is the body of GET /v1/health.
type HealthResponse struct {
	Protocol int    `json:"protocol"`
	Version  string `json:"version"`
	Backend  string `json:"backend"`
	// Via lists the values of Notification.Via this daemon can deliver
	// now: "email" when an email grant is set up.
	Via []string `json:"via"`
}

// ErrorResponse is the body of every non-2xx response.
type ErrorResponse struct {
	Error string `json:"error"`
}

// Normalize fills defaults and rejects what no backend should be handed.
func (n *Notification) Normalize() error {
	if n.Title == "" {
		return errors.New("title is required")
	}
	if n.Urgency == "" {
		n.Urgency = UrgencyNormal
	}

	switch n.Urgency {
	case UrgencyLow, UrgencyNormal, UrgencyCritical:
	default:
		return fmt.Errorf("urgency must be low, normal or critical, not %q", n.Urgency)
	}
	switch n.Via {
	case "", ViaEmail:
	default:
		return fmt.Errorf("via must be empty or %q, not %q", ViaEmail, n.Via)
	}

	fields := []struct {
		name  string
		value string
		limit int
	}{
		{"title", n.Title, maxTitleRunes},
		{"body", n.Body, maxBodyRunes},
		{"source", n.Source, maxSourceRunes},
		{"tag", n.Tag, maxTagRunes},
	}
	for _, field := range fields {
		if !utf8.ValidString(field.value) {
			return fmt.Errorf("%s is not valid UTF-8", field.name)
		}
		// A backend may pass the text as a process argument, which cannot
		// hold a NUL; refuse it as the caller's mistake, not the platform's.
		if strings.ContainsRune(field.value, 0) {
			return fmt.Errorf("%s contains a NUL character", field.name)
		}
		length := utf8.RuneCountInString(field.value)
		if length > field.limit {
			return fmt.Errorf("%s is %d characters; the limit is %d", field.name, length, field.limit)
		}
	}
	return nil
}
