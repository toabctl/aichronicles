package web

import (
	"database/sql"
	"html"
	"time"

	"github.com/toabctl/aichronicles/internal/store"
)

// activityWindow defines how recent a session's last event must be
// for the row to render as "active". Five minutes covers normal
// pause-to-think cadence (a user reading model output, switching
// tabs) without claiming Claude is active for a session that
// crashed without firing SessionEnd.
const activityWindow = 5 * time.Minute

// sessionStatus categorises a session into one of three buckets the
// sessions-list UI surfaces as a coloured dot. ended_at_ms wins
// over recency: a session can be both ended and stale, but the
// user cares first that it's done. title is a tooltip the dot
// carries via the title attribute.
//
// Statuses:
//   - "ended"  → ended_at_ms set; tooltip names the end time.
//   - "active" → still open && most recent event is within
//     activityWindow.
//   - "idle"   → still open but the most recent event is older
//     than activityWindow (likely Claude crashed or the user
//     wandered off without ending the session); also the
//     fallback for a session with no events at all.
func sessionStatus(endedMs sql.NullInt64, latestTsMs int64, now time.Time) (status, title string) {
	if endedMs.Valid && endedMs.Int64 > 0 {
		return "ended", "ended " + relativeTime(endedMs.Int64, now)
	}
	if latestTsMs <= 0 {
		return "idle", "no events yet"
	}
	age := now.Sub(time.UnixMilli(latestTsMs))
	if age < 0 || age >= activityWindow {
		return "idle", "idle — last event " + relativeTime(latestTsMs, now)
	}
	return "active", "active — last event " + relativeTime(latestTsMs, now)
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
func renderLatestEventCell(e store.LiveEvent) string {
	snippet := truncateForStream(e.Snippet.String)
	return `<span class="ts">` + html.EscapeString(time.UnixMilli(e.TsSourceMs).UTC().Format("15:04:05")) + `</span> ` +
		`<span class="badge">` + html.EscapeString(e.Kind) + `</span> ` +
		`<span class="snippet">` + html.EscapeString(snippet) + `</span>`
}

// statusForLiveEvent picks the activity status driven by an
// incoming SSE event. Used by the stream handler when emitting
// per-session updates: a normal event means the session is
// freshly active; the SessionEnd kind flips it to ended.
//
// The "idle" state is intentionally never returned here — idle
// is computed only from passing time, and SSE only fires when
// new activity exists. Decay from active back to idle requires
// a page refresh (documented trade-off in the architecture).
func statusForLiveEvent(e store.LiveEvent, now time.Time) (status, title string) {
	if e.Kind == "session_end" {
		return "ended", "ended " + relativeTime(e.TsSourceMs, now)
	}
	return "active", "active — last event " + relativeTime(e.TsSourceMs, now)
}
