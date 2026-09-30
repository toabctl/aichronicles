package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/wire"
)

// importMaxLineBytes caps a single NDJSON line. One line is one
// envelope; envelopes carrying inlined large tool results can
// legitimately exceed 16 MiB (we have observed real transcripts
// with ~49 MB single lines). 128 MiB matches the per-request
// envelope cap on /v1/ingest so the bulk path accepts the same
// shapes a live hook would.
const importMaxLineBytes = 128 << 20

// importInitialBufferBytes is bufio.Scanner's starting buffer;
// it grows up to importMaxLineBytes on demand. 1 MiB balances
// "most lines fit without realloc" against "small idle import
// doesn't sit on a giant arena."
const importInitialBufferBytes = 1 << 20

// handleImport serves POST /v1/import — bulk NDJSON import of
// envelopes. Each line is parsed as an events.Envelope and
// pushed through the Server's Pipeline (so server-side redaction
// and the extractor registry apply uniformly with /v1/ingest).
//
// Per-line failure mode:
//   - malformed JSON / Validate failure: `invalid++`, line skipped
//   - Pipeline error: returns 500 with stats so far in the body
//     so an operator can see exactly where the run stopped
//
// Response: wire.ImportStats as JSON. 200 even when invalid > 0;
// the client decides whether the rejected count is acceptable.
//
// Content-Type: prefers application/x-ndjson but accepts
// application/json or no Content-Type at all (cobra-CLI clients
// commonly omit it). Any explicit non-NDJSON-compatible content-
// type is rejected with 415.
//
// Deadlines: an import streams its body while it works, so it runs
// far past the server-wide ReadTimeout (which bounds reading the
// WHOLE body) and WriteTimeout. Both used to fire mid-run: the read
// side failed with an i/o timeout partway through the corpus, the
// write side made the partial-stats response undeliverable, and the
// client saw a bare connection error. The write deadline is lifted
// for the run like the other long operations; the read side swaps the
// whole-body bound for an idle bound (idleDeadlineReader), so a long
// import completes while a stalled client still gets cut off.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	clearWriteDeadlineForLongOp(w)
	body := newIdleDeadlineReader(r.Body, http.NewResponseController(w), s.importIdleTimeout)

	if ct := r.Header.Get("Content-Type"); ct != "" && !isAcceptableImportContentType(ct) {
		writeProblem(w, http.StatusUnsupportedMediaType, "Unsupported content-type",
			"expected application/x-ndjson, application/json, or unset")
		return
	}

	start := time.Now()
	stats := wire.ImportStats{}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, importInitialBufferBytes), importMaxLineBytes)

	for scanner.Scan() {
		stats.LinesRead++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}

		// Per-line decode + validate. Either failure increments
		// Invalid and skips the line.
		var env events.Envelope
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&env); err != nil {
			stats.Invalid++
			continue
		}
		if err := env.Validate(); err != nil {
			stats.Invalid++
			continue
		}

		// Pipeline.Process redacts (server-side) and writes
		// through the SQLite Sink. Per-line errors halt the run
		// — a Sink-write failure mid-stream means the storage is
		// in trouble; surface to the operator rather than silently
		// drop the rest of the lines.
		result, err := s.pipeline.Process(r.Context(), events.Event{
			Envelope: &env,
			Raw:      line,
		})
		if err != nil {
			stats.DurationM = time.Since(start).Milliseconds()
			s.slog.Error("import: pipeline process",
				"line", stats.LinesRead, "event_id", env.EventID, "err", err)
			writeProblem(w, http.StatusInternalServerError,
				"Storage error mid-import",
				partialStatsDetail(stats))
			return
		}
		if result.Deduped {
			stats.Deduped++
		} else {
			stats.Imported++
			s.sseBus.Publish(wire.StreamEvent{
				EventID:    result.EventID,
				SessionID:  result.SessionID,
				Kind:       env.Kind,
				TsServerMs: time.Now().UnixMilli(),
			})
		}
	}
	// scanner.Err returns on non-EOF read failures (broken
	// connection, line exceeding importMaxLineBytes). Surface
	// the failure with whatever we managed to write so the
	// client can decide whether to retry.
	if err := scanner.Err(); err != nil {
		stats.DurationM = time.Since(start).Milliseconds()
		s.slog.Error("import: scanner",
			"line", stats.LinesRead, "err", err)
		// bufio.ErrTooLong is the canonical "single line over
		// the buffer cap" failure mode — distinguish it so
		// operators don't blame storage for a malformed input.
		var detail string
		if errors.Is(err, bufio.ErrTooLong) {
			detail = "one line exceeded importMaxLineBytes; " + partialStatsDetail(stats)
		} else {
			detail = err.Error() + "; " + partialStatsDetail(stats)
		}
		writeProblem(w, http.StatusBadRequest, "Import scanner error", detail)
		return
	}

	stats.DurationM = time.Since(start).Milliseconds()
	writeJSON(w, http.StatusOK, stats)
}

// idleDeadlineReader re-arms the connection's read deadline before
// every Read, turning http.Server.ReadTimeout's whole-body bound into
// an idle bound: the request may stream for as long as it keeps
// sending, but a client that sends nothing for idle is cut off with an
// i/o timeout. Time the handler spends processing between reads does
// not count against the client.
type idleDeadlineReader struct {
	r    io.Reader
	rc   *http.ResponseController
	idle time.Duration
}

func newIdleDeadlineReader(r io.Reader, rc *http.ResponseController, idle time.Duration) *idleDeadlineReader {
	return &idleDeadlineReader{r: r, rc: rc, idle: idle}
}

func (d *idleDeadlineReader) Read(p []byte) (int, error) {
	// ErrNotSupported (e.g. an httptest recorder) leaves the
	// server-wide deadline in force — the old behaviour, not a failure.
	_ = d.rc.SetReadDeadline(time.Now().Add(d.idle))
	return d.r.Read(p)
}

// isAcceptableImportContentType returns true when ct is empty,
// application/x-ndjson, application/json (some clients can't
// produce x-ndjson), or text/plain (curl's default). Other types
// are rejected with 415.
func isAcceptableImportContentType(ct string) bool {
	for _, ok := range []string{"application/x-ndjson", "application/json", "text/plain"} {
		if strings.HasPrefix(ct, ok) {
			return true
		}
	}
	return false
}

// partialStatsDetail formats the stats accumulated before a
// failure so the operator sees how far the import got. Used as
// the Detail of the problem+json body when the run aborts mid-
// stream.
func partialStatsDetail(stats wire.ImportStats) string {
	b, _ := json.Marshal(stats)
	return "partial stats: " + string(b)
}
