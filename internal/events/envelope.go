// Package ingest defines the on-the-wire contract for aichronicles.
// The Envelope is the single shape every source (hook, bridge, import) produces;
// daemon handlers, client CLIs, and tests all consume it through this package.
//
// This file is the authoritative Go source of truth. A mirror description
// aimed at third-party agents lives in api/openapi.yaml; keep them in sync.
//
// Reuse: this package is the canonical wire schema. Third parties
// building bridges, importers, or alternate clients should import
// it directly to produce well-formed Envelopes the daemon will
// accept. aichronicles is a work in progress.
package events

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// CurrentSchemaVersion is the only envelope version accepted today.
const CurrentSchemaVersion = 1

// agentSlugPattern enforces stable, URL-friendly source_agent identifiers.
var agentSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// namespace for deriving our session_id from (source_agent, source_session_id).
// Stable across rebuilds because NewSHA1 is deterministic.
var namespace = uuid.NewSHA1(uuid.NameSpaceDNS, []byte("aichronicles.local"))

// Envelope is the wire contract for a single event arriving at /v1/events.
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
	TsSource           time.Time      `json:"ts_source"`
	Cwd                string         `json:"cwd,omitempty"`
	Host               string         `json:"host,omitempty"`
	Tool               *Tool          `json:"tool,omitempty"`
	ContentText        string         `json:"content_text,omitempty"`
	Payload            map[string]any `json:"payload"`
	Transport          string         `json:"transport,omitempty"`
	Redaction          *Redaction     `json:"redaction,omitempty"`
	Subagent           *Subagent      `json:"subagent,omitempty"`

	// Server-assigned on persist. Zero on the wire; populated before logging.
	SessionID string    `json:"session_id,omitempty"`
	TsServer  time.Time `json:"ts_server,omitempty"`
}

// Tool carries normalized tool-invocation metadata when kind is tool_use,
// tool_result, or tool_failure.
type Tool struct {
	Name   string `json:"name,omitempty"`
	CallID string `json:"call_id,omitempty"`
}

// Subagent identifies the sub-agent thread an event belongs to. Nil
// (omitted on the wire) for top-level events run by the main agent.
// ID is the per-session sub-agent identifier the host emits in
// SubagentStart / SubagentStop / nested tool_use payloads; events
// sharing the same (session_id, ID) belong to the same thread. Type
// is the sub-agent's role label (e.g. "planner", "researcher") when
// available; absent for hosts that don't expose one.
//
// Linkage to a parent event id is intentionally NOT modelled here
// in v1: the (session, subagent_id) pair is enough to query "what
// did the planner do," and tracing causal parent events would
// require more host metadata than Claude Code's hooks currently
// expose. Future work — see TODO.md if it's still listed.
type Subagent struct {
	ID   string `json:"id,omitempty"`
	Type string `json:"type,omitempty"`
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
	} else if !IsValidKind(e.Kind) {
		issues = append(issues, fmt.Sprintf("kind %q is not a known canonical kind", e.Kind))
	}
	// role is optional on the wire (assemble fills it from kind), but
	// when set it must be one of the closed values. A bridge sending
	// role="USER" instead of "user" would silently end up in cross-
	// source role queries with no match — refuse explicitly.
	if e.Role != "" && !IsValidRole(e.Role) {
		issues = append(issues, fmt.Sprintf("role %q is not a known canonical role", e.Role))
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
