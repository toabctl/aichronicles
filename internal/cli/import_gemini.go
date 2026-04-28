package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/store"
	"github.com/toabctl/aichronicles/pkg/ingest"
	"github.com/toabctl/aichronicles/pkg/redact"
)

// geminiSourceAgent is the source_agent string we tag every imported
// envelope with — kept distinct from "claude-code" / "codex" so
// downstream filters (`--agent gemini-cli`) work cleanly. Matches
// the binary name the user actually types
// (https://github.com/google-gemini/gemini-cli).
const geminiSourceAgent = "gemini-cli"

// geminiToolEventNamespace is the UUIDv5 namespace we hash
// (parent-message-id, tool-call-id) under to synthesize event_ids
// for tool_use / tool_result events split out of an assistant
// message. Must stay constant across releases.
var geminiToolEventNamespace = uuid.Must(uuid.Parse("9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d"))

func newImportGeminiCmd() *cobra.Command {
	var dbPath string
	cmd := &cobra.Command{
		Use:   "import-gemini [path]",
		Short: "Import gemini-cli session JSON files into the store",
		Long: "Walks one or more gemini-cli session files (one JSON per\n" +
			"conversation, written under ~/.gemini/tmp/<project>/chats/) and\n" +
			"ingests every message — user prompt, assistant turn, tool call,\n" +
			"tool result — as a canonical envelope.\n\n" +
			"event_id is the message's own UUID for user / assistant turns;\n" +
			"tool_use and tool_result events synthesize an id by\n" +
			"UUIDv5(namespace, parentMessageID + tool_call_id + suffix). All\n" +
			"three are stable across re-imports so the operation is\n" +
			"idempotent.\n\n" +
			"path defaults to ~/.gemini/tmp (gemini-cli's per-project root).\n" +
			"A specific session-*.json file or any directory below the root\n" +
			"works too.\n\n" +
			"Trust model: like import-claude, this bypasses the daemon. Edge\n" +
			"redaction runs in-process; anything else the daemon enforces\n" +
			"does not.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := defaultGeminiTmpDir()
			if len(args) == 1 {
				target = args[0]
			}

			s, err := openStore(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			log := newImportGeminiLogger(cmd.ErrOrStderr())
			report, err := ImportGeminiTranscripts(cmd.Context(), target, s, log)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", "",
		"SQLite DB path (overrides $AICHRONICLES_DB; defaults to XDG_STATE_HOME)")
	return cmd
}

// newImportGeminiLogger wraps stderr with a slog.Logger pinned to
// the import-gemini command name, matching the equivalent helper
// for import-claude. Keeping a per-command constructor avoids the
// "cmd=A cmd=B" double-attribute artefact that shows up if you
// just .With("cmd", ...) onto a logger already carrying "cmd".
func newImportGeminiLogger(stderr io.Writer) *slog.Logger {
	h := slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(h).With("cmd", "aichronicles import-gemini")
}

// defaultGeminiTmpDir returns ~/.gemini/tmp — gemini-cli's
// per-project session-state root. Each immediate subdir is one
// project, with sessions in <project>/chats/session-<ts>-<id8>.json.
func defaultGeminiTmpDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".gemini", "tmp")
}

// GeminiImportReport is the per-run summary printed to stdout.
// Mirrors ClaudeImportReport's shape so the user sees a uniform
// "what happened" block regardless of which agent was imported.
type GeminiImportReport struct {
	FilesRead    int
	MessagesRead int
	Imported     int
	Deduped      int
	Invalid      int // file or message-level parse failure
	DurationMS   int64
}

func (r GeminiImportReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "files read:    %d\n", r.FilesRead)
	fmt.Fprintf(&b, "messages read: %d\n", r.MessagesRead)
	fmt.Fprintf(&b, "imported:      %d (envelopes; an assistant turn with N tool calls produces 1+2N envelopes)\n", r.Imported)
	fmt.Fprintf(&b, "deduped:       %d (already in store by event_id)\n", r.Deduped)
	fmt.Fprintf(&b, "invalid:       %d (malformed JSON or missing required fields)\n", r.Invalid)
	fmt.Fprintf(&b, "duration_ms:   %d", r.DurationMS)
	return b.String()
}

// ImportGeminiTranscripts walks target (file or directory) and
// ingests every parseable gemini-cli session file. Idempotent on
// re-run: every event_id is derived from a stable id in the source
// file (message UUID, or UUIDv5 hash for synthesized tool events).
func ImportGeminiTranscripts(ctx context.Context, target string, s *store.Store, log *slog.Logger) (GeminiImportReport, error) {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	start := time.Now()
	report := GeminiImportReport{}
	batcher := newEnvelopeBatcher(s)

	info, err := os.Stat(target)
	if err != nil {
		report.DurationMS = time.Since(start).Milliseconds()
		return report, fmt.Errorf("stat %s: %w", target, err)
	}

	cwdMap := loadGeminiProjectsMap()

	var files []string
	if info.IsDir() {
		err := filepath.WalkDir(target, func(path string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() {
				return nil
			}
			// session files live at <project>/chats/session-<ts>-<id>.json
			if strings.HasSuffix(filepath.Base(path), ".json") &&
				strings.HasPrefix(filepath.Base(path), "session-") &&
				filepath.Base(filepath.Dir(path)) == "chats" {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			report.DurationMS = time.Since(start).Milliseconds()
			return report, fmt.Errorf("walk %s: %w", target, err)
		}
	} else {
		files = []string{target}
	}

	if len(files) > 0 {
		log.Info("import starting", "files", len(files))
	}

	for i, path := range files {
		fileStart := time.Now()
		msgsBefore := report.MessagesRead
		if err := importGeminiFile(ctx, path, batcher, &report, cwdMap); err != nil {
			_ = batcher.Flush(ctx)
			report.Imported = batcher.Imported()
			report.Deduped = batcher.Deduped()
			report.DurationMS = time.Since(start).Milliseconds()
			return report, fmt.Errorf("import %s: %w", path, err)
		}
		log.Info("file done",
			"index", i+1,
			"total", len(files),
			"path", path,
			"messages", report.MessagesRead-msgsBefore,
			"elapsed", time.Since(fileStart),
		)
	}

	if err := batcher.Flush(ctx); err != nil {
		report.Imported = batcher.Imported()
		report.Deduped = batcher.Deduped()
		report.DurationMS = time.Since(start).Milliseconds()
		return report, fmt.Errorf("flush: %w", err)
	}

	report.Imported = batcher.Imported()
	report.Deduped = batcher.Deduped()
	report.DurationMS = time.Since(start).Milliseconds()
	return report, nil
}

// geminiSession mirrors gemini-cli's on-disk session JSON shape.
// Capture the fields we actually consume; unknown fields preserve
// in payload.* via the raw map we re-marshal.
type geminiSession struct {
	SessionID   string          `json:"sessionId"`
	ProjectHash string          `json:"projectHash"`
	StartTime   string          `json:"startTime"`
	LastUpdated string          `json:"lastUpdated"`
	Kind        string          `json:"kind"`
	Messages    []geminiMessage `json:"messages"`
}

// geminiMessage covers user / gemini message rows. Content is
// json.RawMessage because shape differs by type (array of {text}
// for user, plain string for gemini).
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

type geminiProjectsFile struct {
	Projects map[string]string `json:"projects"`
}

// loadGeminiProjectsMap reads ~/.gemini/projects.json and returns
// a label→cwd map (inverted from the on-disk cwd→label shape) so
// the importer can look up "given encoded label, what's the cwd?"
// Empty/missing file returns an empty map (cwd will be unknown,
// not fatal).
func loadGeminiProjectsMap() map[string]string {
	out := map[string]string{}
	home, err := os.UserHomeDir()
	if err != nil {
		return out
	}
	data, err := os.ReadFile(filepath.Join(home, ".gemini", "projects.json"))
	if err != nil {
		return out
	}
	var parsed geminiProjectsFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		return out
	}
	for cwd, label := range parsed.Projects {
		out[label] = cwd
	}
	return out
}

// importGeminiFile reads one session JSON and queues its messages
// onto the batcher. A malformed session file counts as one
// invalid; per-message failures count individually.
func importGeminiFile(ctx context.Context, path string, batcher *envelopeBatcher, report *GeminiImportReport, cwdMap map[string]string) error {
	report.FilesRead++

	cwd := geminiCwdForPath(path, cwdMap)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var sess geminiSession
	if err := json.Unmarshal(data, &sess); err != nil {
		report.Invalid++
		return nil
	}
	if sess.SessionID == "" {
		report.Invalid++
		return nil
	}

	for _, m := range sess.Messages {
		report.MessagesRead++
		envs, err := geminiMessageToEnvelopes(&sess, &m, cwd)
		if err != nil {
			report.Invalid++
			continue
		}
		for _, env := range envs {
			raw, mErr := json.Marshal(env)
			if mErr != nil {
				report.Invalid++
				continue
			}
			if err := batcher.Add(ctx, env, raw); err != nil {
				return fmt.Errorf("%s: ingest msg %s: %w", path, m.ID, err)
			}
		}
	}
	return nil
}

// geminiCwdForPath best-effort resolves the cwd for a session
// file path. Sessions live at ~/.gemini/tmp/<label>/chats/session-*.json;
// the encoded-label maps to a real cwd via projects.json. When the
// path is outside that shape (an explicit file argument, a synthetic
// test fixture), the caller falls back to empty cwd.
func geminiCwdForPath(path string, cwdMap map[string]string) string {
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

// geminiMessageToEnvelopes turns ONE gemini-cli message into one
// or more canonical envelopes. The fan-out:
//   - type=user                 → 1 user_prompt
//   - type=gemini, no toolCalls → 1 assistant_message
//   - type=gemini, N toolCalls  → 1 assistant_message (if content
//     non-empty) + N tool_use + N tool_result envelopes
//
// Returning multiple envelopes is how we preserve the cause-and-
// effect chain in the events table (an assistant turn followed by
// each tool call's request and response). It mirrors how
// import-claude splits transcripts of the same shape.
func geminiMessageToEnvelopes(sess *geminiSession, m *geminiMessage, cwd string) ([]*ingest.Envelope, error) {
	if m.ID == "" {
		return nil, errors.New("missing message id")
	}
	if m.Timestamp == "" {
		return nil, errors.New("missing timestamp")
	}
	ts, err := parseGeminiTime(m.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("parse timestamp %q: %w", m.Timestamp, err)
	}

	switch m.Type {
	case "user":
		text := flattenGeminiUserContent(m.Content)
		return []*ingest.Envelope{
			redactedEnvelope(&ingest.Envelope{
				V:               1,
				EventID:         m.ID,
				SourceAgent:     geminiSourceAgent,
				SourceSessionID: sess.SessionID,
				Kind:            ingest.KindUserPrompt,
				Role:            ingest.RoleUser,
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
			}),
		}, nil

	case "gemini", "model", "assistant":
		var envs []*ingest.Envelope
		text := flattenGeminiAssistantContent(m.Content)
		// One assistant_message envelope when there's actual
		// reply text. Many tool-only turns have empty content
		// because the agent answered exclusively via tool_use.
		if text != "" {
			envs = append(envs, redactedEnvelope(&ingest.Envelope{
				V:               1,
				EventID:         m.ID,
				SourceAgent:     geminiSourceAgent,
				SourceSessionID: sess.SessionID,
				Kind:            ingest.KindAssistantMessage,
				Role:            ingest.RoleAssistant,
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
			}))
		}
		for i, tc := range m.ToolCalls {
			envs = append(envs, geminiToolUseEnvelope(sess, m, &tc, i, cwd, ts))
			if len(tc.Result) > 0 {
				envs = append(envs, geminiToolResultEnvelope(sess, m, &tc, i, cwd, ts))
			}
		}
		return envs, nil

	default:
		// Unknown type → drop it rather than produce a bogus
		// row. report.Invalid is incremented by the caller.
		return nil, fmt.Errorf("unknown message type %q", m.Type)
	}
}

// geminiToolUseEnvelope synthesises one tool_use event from a
// gemini toolCall block. event_id =
// UUIDv5(namespace, parentMessageID + tool_call_id + ":use") so
// it's stable across re-imports without a real UUID on the source.
func geminiToolUseEnvelope(sess *geminiSession, parent *geminiMessage, tc *geminiToolCall, idx int, cwd string, parentTs time.Time) *ingest.Envelope {
	ts := parentTs
	if tc.Timestamp != "" {
		if t, err := parseGeminiTime(tc.Timestamp); err == nil {
			ts = t
		}
	}
	eid := uuid.NewSHA1(geminiToolEventNamespace,
		[]byte(fmt.Sprintf("%s|%s|%d|use", parent.ID, tc.ID, idx))).String()

	argsJSON, _ := json.Marshal(tc.Args)
	return redactedEnvelope(&ingest.Envelope{
		V:               1,
		EventID:         eid,
		SourceAgent:     geminiSourceAgent,
		SourceSessionID: sess.SessionID,
		Kind:            ingest.KindToolUse,
		Role:            ingest.RoleAssistant,
		TsSource:        ts,
		Cwd:             cwd,
		Tool:            &ingest.Tool{Name: tc.Name, CallID: tc.ID},
		ContentText:     tc.Name,
		Payload: map[string]any{
			"sessionId":  sess.SessionID,
			"parent_id":  parent.ID,
			"tool_call":  tc,
			"tool_input": tc.Args,
			"args_json":  string(argsJSON),
		},
		Transport: "import",
	})
}

// geminiToolResultEnvelope mirrors the tool_use envelope but for
// the response side. The result text is the flattened "output"
// field (when present) since that's the high-recall human-readable
// payload; the structured response stays in payload.* for replay.
//
// We add a 1ms epsilon to the result timestamp so it sorts AFTER
// the corresponding tool_use in `ORDER BY ts_source_ms` queries —
// the source format has a single timestamp per toolCall, but
// logically the result is later than the call.
func geminiToolResultEnvelope(sess *geminiSession, parent *geminiMessage, tc *geminiToolCall, idx int, cwd string, parentTs time.Time) *ingest.Envelope {
	ts := parentTs
	if tc.Timestamp != "" {
		if t, err := parseGeminiTime(tc.Timestamp); err == nil {
			ts = t
		}
	}
	ts = ts.Add(time.Millisecond)
	eid := uuid.NewSHA1(geminiToolEventNamespace,
		[]byte(fmt.Sprintf("%s|%s|%d|result", parent.ID, tc.ID, idx))).String()

	text := ""
	if len(tc.Result) > 0 {
		if out, ok := tc.Result[0].FunctionResponse.Response["output"].(string); ok {
			text = out
		}
	}
	kind := ingest.KindToolResult
	if tc.Status == "error" || tc.Status == "failure" {
		kind = ingest.KindToolFailure
	}
	return redactedEnvelope(&ingest.Envelope{
		V:               1,
		EventID:         eid,
		SourceAgent:     geminiSourceAgent,
		SourceSessionID: sess.SessionID,
		Kind:            kind,
		Role:            ingest.RoleTool,
		TsSource:        ts,
		Cwd:             cwd,
		Tool:            &ingest.Tool{Name: tc.Name, CallID: tc.ID},
		ContentText:     text,
		Payload: map[string]any{
			"sessionId": sess.SessionID,
			"parent_id": parent.ID,
			"tool_call": tc,
			"status":    tc.Status,
		},
		Transport: "import",
	})
}

// flattenGeminiUserContent extracts text from a user message's
// content, which on disk is `[{"text":"..."}]` (an array of
// content blocks). Concatenates with newlines if multiple blocks.
func flattenGeminiUserContent(raw json.RawMessage) string {
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
	// Fallback: bare string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

// flattenGeminiAssistantContent extracts text from a gemini
// message's content. The on-disk shape is a plain string for
// gemini turns; we tolerate the array form too in case future
// versions normalise.
func flattenGeminiAssistantContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return flattenGeminiUserContent(raw)
}

// parseGeminiTime accepts the RFC3339 / RFC3339Nano variants
// gemini-cli's writer emits. Currently always RFC3339Nano in
// observed data; the fallback is defensive.
func parseGeminiTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// redactedEnvelope applies edge redaction in-place and returns the
// envelope. Centralised so every envelope path uses the same
// scrubber — a pasted API key in a gemini transcript must never
// reach disk in plain form.
func redactedEnvelope(env *ingest.Envelope) *ingest.Envelope {
	ingest.ApplyRedaction(env, redact.Default())
	return env
}
