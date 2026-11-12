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
	maxRequestSize = 64 << 10
)

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

	fields := []struct {
		name  string
		value string
		limit int
	}{
		{"title", n.Title, maxTitleRunes},
		{"body", n.Body, maxBodyRunes},
		{"source", n.Source, maxSourceRunes},
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
