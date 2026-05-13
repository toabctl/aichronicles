package web

import (
	"html"
	"time"

	"github.com/toabctl/aichronicles/internal/wire"
)

// activityWindow defines how recent a session's last event must be
// for the row to render as "active". Five minutes covers normal
// pause-to-think cadence (a user reading model output, switching
// tabs) without claiming Claude is active for a session that
// crashed without firing SessionEnd.
const activityWindow = 5 * time.Minute

// sessionStatus categorises a session into one of three buckets the
// sessions-list UI surfaces as a coloured dot. The signal is
// derived from the session's most recent event: the kind tells us
// whether the session has formally ended (session_end), and the
// timestamp tells us whether it's still humming along. title is a
// tooltip the dot carries via the title attribute.
//
// Why not check sessions.ended_at_ms? The INSERT trigger on
// `events` updates ended_at_ms to MAX(existing, new.ts_source_ms)
// for every event, so the column is always populated and means
// "timestamp of latest event", not "session formally ended". The
// SessionEnd hook fires a kind='session_end' event — that's the
// real signal.
//
// Statuses:
//   - "ended"  → latest event kind is session_end.
//   - "active" → latest event is within activityWindow and not
//     a session_end.
//   - "idle"   → latest event is older than activityWindow (or
//     missing entirely): Claude probably crashed or the user
//     wandered off without ending cleanly.
//
// latest may be nil for a session with no events yet; in that
// case the fallback is "idle" with a tooltip distinguishing it
// from a stale session.
func sessionStatus(latest *wire.Event, now time.Time) (status, title string) {
	if latest == nil || latest.TsSourceMs <= 0 {
		return "idle", "no events yet"
	}
	if latest.Kind == "session_end" {
		return "ended", "ended " + relativeTime(latest.TsSourceMs, now)
	}
	age := now.Sub(time.UnixMilli(latest.TsSourceMs))
	if age < 0 || age >= activityWindow {
		return "idle", "idle — last event " + relativeTime(latest.TsSourceMs, now)
	}
	return "active", "active — last event " + relativeTime(latest.TsSourceMs, now)
}

// renderStatusDot produces the coloured status dot for one session.
// Same Go-side renderer is called by:
//
//   - loadSessionsForList for the initial server-side page render
//     (oob=false → plain span the template embeds in a cell)
//   - the SSE streamHandler when emitting per-session updates
//     (oob=true → adds hx-swap-oob so htmx swaps the dot in by id)
//
// Sharing the renderer guarantees the markup is byte-identical
// across the two entry points, which is what makes the dot
// visually stable under live updates.
func renderStatusDot(sessionID, status, title string, oob bool) string {
	oobAttr := ""
	if oob {
		oobAttr = ` hx-swap-oob="true"`
	}
	return `<span id="status-` + html.EscapeString(sessionID) + `"` +
		oobAttr +
		` class="status status-` + html.EscapeString(status) + `"` +
		` title="` + html.EscapeString(title) + `">●</span>`
}

// renderLatestEventCell produces the inner HTML for the
// "Latest event" cell — ts + kind badge + snippet, single-line
// so an SSE data field can carry it without splitting. No
// session-id link in here: the row already has one, and an
// extra link inside the cell would be visual noise.
//
// As with renderStatusDot, the same renderer powers initial
// page render and live SSE updates so the cell looks identical
// before and after a swap.
func renderLatestEventCell(e wire.Event) string {
	snippet := truncateForStream(derefOr(e.Snippet, ""))
	kind := html.EscapeString(e.Kind)
	return `<span class="ts">` + html.EscapeString(time.UnixMilli(e.TsSourceMs).UTC().Format("15:04:05")) + `</span> ` +
		`<span class="badge badge-` + kind + `">` + kind + `</span> ` +
		`<span class="snippet">` + html.EscapeString(snippet) + `</span>`
}
