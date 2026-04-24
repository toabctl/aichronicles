// Package ingest defines the on-the-wire contract for aichronicles.
// The Envelope is the single shape every source (hook, bridge, import) produces;
// daemon handlers, client CLIs, and tests all consume it through this package.
package ingest

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CurrentSchemaVersion is the only envelope version accepted today.
// Breaking changes bump this and are gated by URL version (/v2/...).
const CurrentSchemaVersion = 1

// agentSlugPattern enforces stable, URL-friendly source_agent identifiers.
var agentSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// namespace for deriving our session_id from (source_agent, source_session_id).
// Stable across rebuilds because NewSHA1 is deterministic.
var namespace = uuid.NewSHA1(uuid.NameSpaceDNS, []byte("aichronicles.local"))

// Envelope is the wire contract for a single event arriving at /v1/ingest.
// Fields tagged omitempty are optional; validation enforces only the
// seven required ones plus format rules.
type Envelope struct {
	V                  int            `json:"v"`
	EventID            string         `json:"event_id"`
	SourceAgent        string         `json:"source_agent"`
	SourceAgentVersion string         `json:"source_agent_version,omitempty"`
	SourceSessionID    string         `json:"source_session_id"`
	Kind               string         `json:"kind"`
	Role               string         `json:"role,omitempty"`
	RoleRaw            string         `json:"role_raw,omitempty"`
	TsSource           time.Time      `json:"ts_source"`
	Cwd                string         `json:"cwd,omitempty"`
	Host               string         `json:"host,omitempty"`
	Tool               *Tool          `json:"tool,omitempty"`
	ContentText        string         `json:"content_text,omitempty"`
	Payload            map[string]any `json:"payload"`
	Transport          string         `json:"transport,omitempty"`
	Redaction          *Redaction     `json:"redaction,omitempty"`

	// Server-assigned on persist. Zero on the wire; populated before logging.
	SessionID string    `json:"session_id,omitempty"`
	TsServer  time.Time `json:"ts_server,omitempty"`
}

// Tool carries normalized tool-invocation metadata when kind is tool_use,
// tool_result, or tool_failure.
type Tool struct {
	Name    string `json:"name,omitempty"`
	NameRaw string `json:"name_raw,omitempty"`
	CallID  string `json:"call_id,omitempty"`
}

// Redaction records what the ingest-edge scrubber did, for audit + tuning.
type Redaction struct {
	Applied  bool     `json:"applied"`
	Patterns []string `json:"patterns,omitempty"`
}

// Ack is returned from /v1/ingest for every accepted envelope.
type Ack struct {
	EventID   string `json:"event_id"`
	SessionID string `json:"session_id"`
	Deduped   bool   `json:"deduped"`
}

// ValidationError groups every problem found in one validation pass so
// callers can surface them together rather than one-at-a-time.
type ValidationError struct {
	Issues []string
}

func (e *ValidationError) Error() string {
	return strings.Join(e.Issues, "; ")
}

// ErrInvalid is returned wrapped in a ValidationError when Validate rejects
// an envelope. Callers use errors.Is to detect bad input.
var ErrInvalid = errors.New("invalid envelope")

// Validate checks the required fields and format rules. It collects every
// issue into one error so a client can fix them all in one round trip.
func (e *Envelope) Validate() error {
	var issues []string

	if e.V != CurrentSchemaVersion {
		issues = append(issues, fmt.Sprintf("v must be %d, got %d", CurrentSchemaVersion, e.V))
	}
	if _, err := uuid.Parse(e.EventID); err != nil {
		issues = append(issues, "event_id must be a UUID")
	}
	if !agentSlugPattern.MatchString(e.SourceAgent) {
		issues = append(issues, "source_agent must match "+agentSlugPattern.String())
	}
	if e.SourceSessionID == "" {
		issues = append(issues, "source_session_id is required")
	}
	if e.Kind == "" {
		issues = append(issues, "kind is required")
	}
	if e.TsSource.IsZero() {
		issues = append(issues, "ts_source is required")
	}
	if e.Payload == nil {
		issues = append(issues, "payload is required (may be empty {})")
	}

	if len(issues) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrInvalid, &ValidationError{Issues: issues})
}

// DeriveSessionID produces our stable session identifier from the source's
// native identifiers. UUIDv5 over (agent:source_session_id) under our
// namespace — deterministic, so re-ingest of the same session yields the
// same ID. Returns a UUID string suitable for storage and logs.
func DeriveSessionID(agent, sourceSessionID string) string {
	return uuid.NewSHA1(namespace, []byte(agent+":"+sourceSessionID)).String()
}
