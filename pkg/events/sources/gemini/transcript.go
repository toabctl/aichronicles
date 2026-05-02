package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/toabctl/aichronicles/pkg/events"
)

const sourceAgent = "gemini-cli"

// toolEventNamespace is the UUIDv5 namespace we hash
// (parent-message-id, tool-call-id, suffix) under to synthesize
// event_ids for tool_use / tool_result events split out of an
// assistant message. Must stay constant across releases.
var toolEventNamespace = uuid.Must(uuid.Parse("9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d"))

// TranscriptSource walks gemini-cli's session JSON files (one JSON
// per conversation, written under ~/.gemini/tmp/<project>/chats/)
// and yields canonical events.Events. One message can fan out into
// multiple envelopes — a gemini turn with N tool calls produces
// 1 assistant_message (when content non-empty) + N tool_use + N
// tool_result envelopes.
//
// The cwd-recovery shim from import_gemini.go is preserved: when
// Root walks ~/.gemini/tmp/<label>/chats/*.json, we look up the
// label in ~/.gemini/projects.json to recover the original cwd.
// CwdMap can be supplied directly by tests; nil falls back to
// reading ~/.gemini/projects.json at iteration time.
type TranscriptSource struct {
	Root     string
	Redactor events.Redactor
	Logger   *slog.Logger
	CwdMap   map[string]string
	Now      func() time.Time

	Stats TranscriptStats
}

// TranscriptStats aggregates per-iteration counters. Reset on each
// call to Events.
type TranscriptStats struct {
	FilesRead    int
	MessagesRead int
	Invalid      int
}

// Events implements events.Source.
func (s *TranscriptSource) Events(ctx context.Context) iter.Seq2[events.Event, error] {
	s.Stats = TranscriptStats{}
	logger := s.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	cwdMap := s.CwdMap
	if cwdMap == nil {
		cwdMap = loadProjectsMap()
	}
	return func(yield func(events.Event, error) bool) {
		if s.Root == "" {
			return
		}
		info, err := os.Stat(s.Root)
		if err != nil {
			yield(events.Event{}, fmt.Errorf("stat %s: %w", s.Root, err))
			return
		}
		var files []string
		if info.IsDir() {
			err := filepath.WalkDir(s.Root, func(path string, d fs.DirEntry, werr error) error {
				if werr != nil {
					return werr
				}
				if d.IsDir() {
					return nil
				}
				if strings.HasSuffix(path, ".json") {
					files = append(files, path)
				}
				return nil
			})
			if err != nil {
				yield(events.Event{}, fmt.Errorf("walk %s: %w", s.Root, err))
				return
			}
		} else {
			files = []string{s.Root}
		}

		for _, path := range files {
			if ctx.Err() != nil {
				yield(events.Event{}, ctx.Err())
				return
			}
			if !s.iterateFile(ctx, path, cwdMap, logger, yield) {
				return
			}
		}
	}
}

func (s *TranscriptSource) iterateFile(
	ctx context.Context,
	path string,
	cwdMap map[string]string,
	logger *slog.Logger,
	yield func(events.Event, error) bool,
) bool {
	s.Stats.FilesRead++
	cwd := cwdForPath(path, cwdMap)
	data, err := os.ReadFile(path)
	if err != nil {
		return yield(events.Event{}, fmt.Errorf("read %s: %w", path, err))
	}
	var sess geminiSession
	if err := json.Unmarshal(data, &sess); err != nil {
		s.Stats.Invalid++
		logger.Warn("invalid session JSON", "path", path, "err", err)
		return true
	}
	if sess.SessionID == "" {
		s.Stats.Invalid++
		logger.Warn("session missing sessionId", "path", path)
		return true
	}

	for _, m := range sess.Messages {
		if ctx.Err() != nil {
			return yield(events.Event{}, ctx.Err())
		}
		s.Stats.MessagesRead++
		envs, err := messageToEnvelopes(&sess, &m, cwd, s.Redactor)
		if err != nil {
			s.Stats.Invalid++
			logger.Warn("convert message",
				"path", path, "message_id", m.ID, "err", err)
			continue
		}
		for _, env := range envs {
			raw, mErr := json.Marshal(env)
			if mErr != nil {
				s.Stats.Invalid++
				logger.Warn("marshal envelope",
					"path", path, "message_id", m.ID, "err", mErr)
				continue
			}
			if err := env.Validate(); err != nil {
				s.Stats.Invalid++
				logger.Warn("validate envelope",
					"path", path, "message_id", m.ID, "err", err)
				continue
			}
			if !yield(events.Event{Envelope: env, Raw: raw}, nil) {
				return false
			}
		}
	}
	return true
}

// geminiSession mirrors gemini-cli's on-disk session JSON shape.
type geminiSession struct {
	SessionID   string          `json:"sessionId"`
	ProjectHash string          `json:"projectHash"`
	StartTime   string          `json:"startTime"`
	LastUpdated string          `json:"lastUpdated"`
	Kind        string          `json:"kind"`
	Messages    []geminiMessage `json:"messages"`
}

type geminiMessage struct {
	ID        string           `json:"id"`
	Timestamp string           `json:"timestamp"`
	Type      string           `json:"type"`
	Content   json.RawMessage  `json:"content"`
	ToolCalls []geminiToolCall `json:"toolCalls,omitempty"`
	Model     string           `json:"model,omitempty"`
}

type geminiToolCall struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	Args      map[string]any     `json:"args"`
	Result    []geminiToolResult `json:"result"`
	Status    string             `json:"status"`
	Timestamp string             `json:"timestamp"`
}

type geminiToolResult struct {
	FunctionResponse struct {
		ID       string         `json:"id"`
		Name     string         `json:"name"`
		Response map[string]any `json:"response"`
	} `json:"functionResponse"`
}

type projectsFile struct {
	Projects map[string]string `json:"projects"`
}

// loadProjectsMap reads ~/.gemini/projects.json and returns a
// label→cwd map (inverted from the on-disk cwd→label shape).
// Empty/missing file returns an empty map.
func loadProjectsMap() map[string]string {
	out := map[string]string{}
	home, err := os.UserHomeDir()
	if err != nil {
		return out
	}
	data, err := os.ReadFile(filepath.Join(home, ".gemini", "projects.json"))
	if err != nil {
		return out
	}
	var parsed projectsFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		return out
	}
	for cwd, label := range parsed.Projects {
		out[label] = cwd
	}
	return out
}

// cwdForPath best-effort resolves the cwd for a session file path.
// Sessions live at ~/.gemini/tmp/<label>/chats/session-*.json; the
// encoded-label maps to a real cwd via projects.json.
func cwdForPath(path string, cwdMap map[string]string) string {
	chatsDir := filepath.Dir(path)
	if filepath.Base(chatsDir) != "chats" {
		return ""
	}
	projectDir := filepath.Dir(chatsDir)
	label := filepath.Base(projectDir)
	if cwd, ok := cwdMap[label]; ok && cwd != "" {
		return cwd
	}
	return ""
}

// messageToEnvelopes turns ONE gemini-cli message into one or more
// canonical envelopes. Redaction is applied per envelope before
// returning. Fan-out:
//   - type=user                 → 1 user_prompt
//   - type=gemini, no toolCalls → 1 assistant_message
//   - type=gemini, N toolCalls  → 1 assistant_message (if content
//     non-empty) + N tool_use + N tool_result envelopes
func messageToEnvelopes(sess *geminiSession, m *geminiMessage, cwd string, redactor events.Redactor) ([]*events.Envelope, error) {
	if m.ID == "" {
		return nil, errors.New("missing message id")
	}
	if m.Timestamp == "" {
		return nil, errors.New("missing timestamp")
	}
	ts, err := parseTime(m.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("parse timestamp %q: %w", m.Timestamp, err)
	}

	switch m.Type {
	case "user":
		text := flattenUserContent(m.Content)
		env := &events.Envelope{
			V:               events.CurrentSchemaVersion,
			EventID:         m.ID,
			SourceAgent:     sourceAgent,
			SourceSessionID: sess.SessionID,
			Kind:            events.KindUserPrompt,
			Role:            events.RoleUser,
			TsSource:        ts,
			Cwd:             cwd,
			ContentText:     text,
			Payload: map[string]any{
				"sessionId": sess.SessionID,
				"messageId": m.ID,
				"timestamp": m.Timestamp,
				"type":      m.Type,
				"content":   json.RawMessage(m.Content),
			},
			Transport: "import",
		}
		applyRedaction(env, redactor)
		return []*events.Envelope{env}, nil

	case "gemini", "model", "assistant":
		var envs []*events.Envelope
		text := flattenAssistantContent(m.Content)
		if text != "" {
			env := &events.Envelope{
				V:               events.CurrentSchemaVersion,
				EventID:         m.ID,
				SourceAgent:     sourceAgent,
				SourceSessionID: sess.SessionID,
				Kind:            events.KindAssistantMessage,
				Role:            events.RoleAssistant,
				TsSource:        ts,
				Cwd:             cwd,
				ContentText:     text,
				Payload: map[string]any{
					"sessionId": sess.SessionID,
					"messageId": m.ID,
					"timestamp": m.Timestamp,
					"type":      m.Type,
					"model":     m.Model,
					"content":   json.RawMessage(m.Content),
				},
				Transport: "import",
			}
			applyRedaction(env, redactor)
			envs = append(envs, env)
		}
		for i, tc := range m.ToolCalls {
			envs = append(envs, toolUseEnvelope(sess, m, &tc, i, cwd, ts, redactor))
			if len(tc.Result) > 0 {
				envs = append(envs, toolResultEnvelope(sess, m, &tc, i, cwd, ts, redactor))
			}
		}
		return envs, nil

	default:
		return nil, fmt.Errorf("unknown message type %q", m.Type)
	}
}

func toolUseEnvelope(sess *geminiSession, parent *geminiMessage, tc *geminiToolCall, idx int, cwd string, parentTs time.Time, redactor events.Redactor) *events.Envelope {
	ts := parentTs
	if tc.Timestamp != "" {
		if t, err := parseTime(tc.Timestamp); err == nil {
			ts = t
		}
	}
	eid := uuid.NewSHA1(toolEventNamespace,
		fmt.Appendf(nil, "%s|%s|%d|use", parent.ID, tc.ID, idx)).String()
	argsJSON, _ := json.Marshal(tc.Args)
	env := &events.Envelope{
		V:               events.CurrentSchemaVersion,
		EventID:         eid,
		SourceAgent:     sourceAgent,
		SourceSessionID: sess.SessionID,
		Kind:            events.KindToolUse,
		Role:            events.RoleAssistant,
		TsSource:        ts,
		Cwd:             cwd,
		Tool:            &events.Tool{Name: tc.Name, CallID: tc.ID},
		ContentText:     tc.Name,
		Payload: map[string]any{
			"sessionId":  sess.SessionID,
			"parent_id":  parent.ID,
			"tool_call":  tc,
			"tool_input": tc.Args,
			"args_json":  string(argsJSON),
		},
		Transport: "import",
	}
	applyRedaction(env, redactor)
	return env
}

func toolResultEnvelope(sess *geminiSession, parent *geminiMessage, tc *geminiToolCall, idx int, cwd string, parentTs time.Time, redactor events.Redactor) *events.Envelope {
	ts := parentTs
	if tc.Timestamp != "" {
		if t, err := parseTime(tc.Timestamp); err == nil {
			ts = t
		}
	}
	ts = ts.Add(time.Millisecond)
	eid := uuid.NewSHA1(toolEventNamespace,
		fmt.Appendf(nil, "%s|%s|%d|result", parent.ID, tc.ID, idx)).String()

	text := ""
	if len(tc.Result) > 0 {
		if out, ok := tc.Result[0].FunctionResponse.Response["output"].(string); ok {
			text = out
		}
	}
	kind := events.KindToolResult
	if tc.Status == "error" || tc.Status == "failure" {
		kind = events.KindToolFailure
	}
	env := &events.Envelope{
		V:               events.CurrentSchemaVersion,
		EventID:         eid,
		SourceAgent:     sourceAgent,
		SourceSessionID: sess.SessionID,
		Kind:            kind,
		Role:            events.RoleTool,
		TsSource:        ts,
		Cwd:             cwd,
		Tool:            &events.Tool{Name: tc.Name, CallID: tc.ID},
		ContentText:     text,
		Payload: map[string]any{
			"sessionId": sess.SessionID,
			"parent_id": parent.ID,
			"tool_call": tc,
			"status":    tc.Status,
		},
		Transport: "import",
	}
	applyRedaction(env, redactor)
	return env
}

// flattenUserContent extracts text from a user message's content,
// which on disk is `[{"text":"..."}]`. Concatenates with newlines
// if multiple blocks.
func flattenUserContent(raw json.RawMessage) string {
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if t, ok := b["text"].(string); ok && t != "" {
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(t)
			}
		}
		return sb.String()
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

// flattenAssistantContent extracts text from a gemini message's
// content. The on-disk shape is a plain string for gemini turns;
// we tolerate the array form too in case future versions
// normalise.
func flattenAssistantContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return flattenUserContent(raw)
}

// parseTime accepts the RFC3339 / RFC3339Nano variants gemini-cli's
// writer emits. Currently always RFC3339Nano in observed data; the
// fallback is defensive.
func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// applyRedaction is a small wrapper that no-ops on a nil Redactor
// (test fixtures) and otherwise calls Apply, populating
// env.Redaction.Applied.
func applyRedaction(env *events.Envelope, redactor events.Redactor) {
	if redactor == nil {
		return
	}
	redactor.Apply(env)
}
