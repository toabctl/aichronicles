package claude

import (
	"bufio"
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

// MaxLineBytes is a sanity bound on a single transcript line. Real
// assistant turns can be huge (50 MB+ is not unheard of when a
// tool result gets inlined), so we read line-by-line via a bufio
// Reader instead of a Scanner — the Scanner's token cap is
// exactly the failure mode this constant defends against when it
// does trip. Any line above this is logged + counted as Invalid
// and skipped rather than aborting the whole walk.
const MaxLineBytes = 128 << 20

// claudeCanonicalTypes are the Claude transcript entry types that
// carry conversational content we convert to canonical envelopes.
// Every other type observed in ~/.claude/projects/*.jsonl is
// Claude-internal bookkeeping (file-history-snapshot,
// permission-mode, queue-operation, attachment, last-prompt) that
// only exists in the on-disk transcript, NOT in the hook event
// stream — none of them unlock queries we can't already answer
// from canonical events, so we skip them silently.
var claudeCanonicalTypes = map[string]struct{}{
	"user":      {},
	"assistant": {},
	"system":    {},
}

// JSONLSource walks Claude Code's transcript files (the .jsonl
// files under ~/.claude/projects) and yields one events.Event per
// conversational line. Implements events.Source.
//
// Configuration:
//
//   - Root: file path (one .jsonl) or directory (recursively
//     walked). Empty Root yields no events.
//   - Logger: receives Warn records for skipped lines (missing
//     uuid, malformed JSON, oversize); nil silences. Skips do NOT
//     halt iteration.
//   - Now: injectable time source for tests. Unused for envelope
//     timestamps (those come from the transcript line's own
//     `timestamp` field), but reserved for future use.
//
// Translation is pure — redaction is performed by the consuming
// events.Pipeline (single point of enforcement). The Source emits
// raw envelopes; the Pipeline scrubs and re-marshals before the
// Sink sees them.
//
// After Events() has been fully consumed, Stats holds counters the
// caller can read: FilesRead, LinesRead, SkippedMissingUUID,
// Invalid. These were previously exposed as ClaudeImportReport
// fields populated inline; here they live on the Source so the
// CLI subcommand can copy them onto its report after Pipeline.Run
// returns.
type JSONLSource struct {
	Root   string
	Logger *slog.Logger
	Now    func() time.Time

	Stats JSONLStats
}

// JSONLStats aggregates per-iteration counters the JSONLSource
// keeps as it walks. Reset on each call to Events.
type JSONLStats struct {
	FilesRead          int
	LinesRead          int
	SkippedMissingUUID int
	Invalid            int
}

// Events yields one events.Event per conversational transcript
// line. Per-line skips (oversize, malformed JSON, missing/bad
// uuid, non-canonical types, validate failures) are logged via
// Logger and counted in Stats — they do NOT halt iteration. Only
// filesystem-level errors (e.g. unreadable directory) yield a
// (zero-Event, error) pair.
func (s *JSONLSource) Events(ctx context.Context) iter.Seq2[events.Event, error] {
	s.Stats = JSONLStats{}
	logger := s.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
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
				if strings.HasSuffix(path, ".jsonl") {
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
			if !s.iterateFile(ctx, path, logger, yield) {
				return
			}
		}
	}
}

// iterateFile walks one .jsonl file. Returns false when the
// outer iterator should halt (yield returned false or filesystem
// error). Per-line errors are logged + counted, never propagated
// up.
func (s *JSONLSource) iterateFile(
	ctx context.Context,
	path string,
	logger *slog.Logger,
	yield func(events.Event, error) bool,
) bool {
	f, err := os.Open(path)
	if err != nil {
		return yield(events.Event{}, fmt.Errorf("open %s: %w", path, err))
	}
	defer func() { _ = f.Close() }()
	s.Stats.FilesRead++

	br := bufio.NewReaderSize(f, 1<<20)
	var lineNum int
	for {
		if ctx.Err() != nil {
			return yield(events.Event{}, ctx.Err())
		}
		line, readErr := br.ReadBytes('\n')
		if len(line) > 0 {
			lineNum++
			s.Stats.LinesRead++
			if !s.processLine(path, lineNum, line, logger, yield) {
				return false
			}
		}
		if readErr == io.EOF {
			return true
		}
		if readErr != nil {
			return yield(events.Event{}, fmt.Errorf("read %s: %w", path, readErr))
		}
	}
}

// processLine handles one transcript line. Returns false only when
// yield returned false (caller wants to stop). Format / validation
// issues are counted and logged; they do NOT propagate.
func (s *JSONLSource) processLine(
	path string,
	lineNum int,
	line []byte,
	logger *slog.Logger,
	yield func(events.Event, error) bool,
) bool {
	if len(line) > MaxLineBytes {
		s.Stats.Invalid++
		logger.Warn("line too large",
			"path", path, "line", lineNum,
			"bytes", len(line), "cap", MaxLineBytes)
		return true
	}
	line = bytesStripTrailingNewline(line)
	if len(strings.TrimSpace(string(line))) == 0 {
		return true
	}

	var entry claudeEntry
	if err := json.Unmarshal(line, &entry); err != nil {
		s.Stats.Invalid++
		logger.Warn("invalid JSON", "path", path, "line", lineNum, "err", err)
		return true
	}
	if _, canonical := claudeCanonicalTypes[entry.Type]; !canonical {
		// Claude-internal bookkeeping row — silent skip.
		return true
	}
	if entry.UUID == "" {
		s.Stats.SkippedMissingUUID++
		logger.Warn("conversational row without uuid",
			"path", path, "line", lineNum, "type", entry.Type)
		return true
	}
	if _, err := uuid.Parse(entry.UUID); err != nil {
		s.Stats.SkippedMissingUUID++
		logger.Warn("malformed uuid",
			"path", path, "line", lineNum, "uuid", entry.UUID)
		return true
	}

	env, rawForStore, err := s.transcriptEntryToEnvelope(&entry, line)
	if err != nil {
		s.Stats.Invalid++
		logger.Warn("convert envelope", "path", path, "line", lineNum, "err", err)
		return true
	}
	if err := env.Validate(); err != nil {
		s.Stats.Invalid++
		logger.Warn("envelope validate", "path", path, "line", lineNum, "err", err)
		return true
	}

	return yield(events.Event{Envelope: env, Raw: rawForStore}, nil)
}

// claudeEntry is the subset of a transcript line we care about.
// The raw line is preserved separately and re-marshaled into the
// canonical Envelope form so source-native fields survive.
type claudeEntry struct {
	Type      string         `json:"type"`
	UUID      string         `json:"uuid"`
	SessionID string         `json:"sessionId"`
	Timestamp string         `json:"timestamp"`
	CWD       string         `json:"cwd"`
	Version   string         `json:"version"`
	Message   *claudeMessage `json:"message"`
	ToolUseID string         `json:"toolUseID"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Model   string          `json:"model"`
}

// transcriptEntryToEnvelope converts one Claude transcript entry
// into a canonical Envelope. The raw transcript line is reparsed
// into a generic map so events.IngestEnvelope-equivalent code sees
// the same JSON it would from a hook. Redaction is applied here
// (matching HookTranslator's contract — sources are the edge);
// the redacted envelope is what gets stored, so a transcript
// containing a pasted API key never reaches disk in plain form.
func (s *JSONLSource) transcriptEntryToEnvelope(entry *claudeEntry, rawLine []byte) (*events.Envelope, []byte, error) {
	if entry.SessionID == "" {
		return nil, nil, errors.New("missing sessionId")
	}
	if entry.Timestamp == "" {
		return nil, nil, errors.New("missing timestamp")
	}
	ts, err := time.Parse(time.RFC3339Nano, entry.Timestamp)
	if err != nil {
		return nil, nil, fmt.Errorf("parse timestamp %q: %w", entry.Timestamp, err)
	}
	kind, role, contentText, tool := classifyClaudeEntry(entry)

	var payload map[string]any
	if err := json.Unmarshal(rawLine, &payload); err != nil {
		return nil, nil, fmt.Errorf("reparse line to payload: %w", err)
	}

	env := &events.Envelope{
		V:                  events.CurrentSchemaVersion,
		EventID:            entry.UUID,
		SourceAgent:        "claude-code",
		SourceAgentVersion: entry.Version,
		SourceSessionID:    entry.SessionID,
		Kind:               kind,
		Role:               role,
		TsSource:           ts,
		Cwd:                entry.CWD,
		Tool:               tool,
		ContentText:        contentText,
		Payload:            payload,
		Transport:          "import",
	}

	// Re-marshal deterministically so the bytes the Pipeline sees
	// (and re-redacts before Sink.Write) are our canonical envelope
	// JSON, not Claude's wire format. The Pipeline owns redaction
	// and will re-marshal the post-redaction envelope, so this
	// initial marshal is just to populate Event.Raw with a
	// well-formed pre-redaction shape.
	out, err := json.Marshal(env)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal envelope: %w", err)
	}
	return env, out, nil
}

// classifyClaudeEntry picks a canonical kind, role, content_text,
// and optional Tool from a transcript entry. Heuristics:
//
//   - user      + string content                → user_prompt
//   - user      + array w/ tool_result          → tool_result
//   - user      + array (text only)             → user_prompt
//   - assistant + array w/ tool_use             → tool_use
//   - assistant + array (text only)             → assistant_message
//   - system                                    → system_message
func classifyClaudeEntry(entry *claudeEntry) (kind, role, contentText string, tool *events.Tool) {
	switch entry.Type {
	case "system":
		return events.KindSystemMessage, events.RoleSystem, stringContent(entry.Message), nil
	case "user":
		return classifyUserEntry(entry)
	case "assistant":
		return classifyAssistantEntry(entry)
	}
	return events.KindUnknown, "", stringContent(entry.Message), nil
}

func classifyUserEntry(entry *claudeEntry) (string, string, string, *events.Tool) {
	if entry.Message == nil || len(entry.Message.Content) == 0 {
		return events.KindUserPrompt, events.RoleUser, "", nil
	}
	var asString string
	if err := json.Unmarshal(entry.Message.Content, &asString); err == nil {
		return events.KindUserPrompt, events.RoleUser, asString, nil
	}
	blocks := parseBlocks(entry.Message.Content)
	if first, ok := firstBlockOfType(blocks, "tool_result"); ok {
		callID, _ := first["tool_use_id"].(string)
		return events.KindToolResult, events.RoleTool,
			flattenToolResultContent(first["content"]),
			&events.Tool{CallID: callID}
	}
	return events.KindUserPrompt, events.RoleUser, joinTextBlocks(blocks), nil
}

func classifyAssistantEntry(entry *claudeEntry) (string, string, string, *events.Tool) {
	if entry.Message == nil || len(entry.Message.Content) == 0 {
		return events.KindAssistantMessage, events.RoleAssistant, "", nil
	}
	blocks := parseBlocks(entry.Message.Content)
	if first, ok := firstBlockOfType(blocks, "tool_use"); ok {
		name, _ := first["name"].(string)
		callID, _ := first["id"].(string)
		return events.KindToolUse, events.RoleTool, name,
			&events.Tool{Name: name, CallID: callID}
	}
	return events.KindAssistantMessage, events.RoleAssistant, joinTextBlocks(blocks), nil
}

func parseBlocks(raw json.RawMessage) []map[string]any {
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	return blocks
}

func firstBlockOfType(blocks []map[string]any, t string) (map[string]any, bool) {
	for _, b := range blocks {
		if typ, _ := b["type"].(string); typ == t {
			return b, true
		}
	}
	return nil, false
}

// joinTextBlocks concatenates "text" and "thinking" blocks' text
// for FTS indexing. Tool-call structures aren't flattened here —
// their metadata lives on events.Tool.
func joinTextBlocks(blocks []map[string]any) string {
	var b strings.Builder
	for _, block := range blocks {
		switch block["type"] {
		case "text":
			if s, ok := block["text"].(string); ok {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(s)
			}
		case "thinking":
			if s, ok := block["thinking"].(string); ok {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(s)
			}
		}
	}
	return b.String()
}

// flattenToolResultContent renders whatever a tool returned into
// a searchable string. tool_result.content can be a string, an
// array of content blocks, or omitted.
func flattenToolResultContent(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []any:
		var b strings.Builder
		for _, item := range x {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := m["text"].(string); ok {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(s)
			}
		}
		return b.String()
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

func stringContent(m *claudeMessage) string {
	if m == nil || len(m.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	return joinTextBlocks(parseBlocks(m.Content))
}

func bytesStripTrailingNewline(line []byte) []byte {
	n := len(line)
	if n > 0 && line[n-1] == '\n' {
		n--
	}
	if n > 0 && line[n-1] == '\r' {
		n--
	}
	return line[:n]
}
