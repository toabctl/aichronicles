package web

// Page is the common envelope every route's view data sits inside.
// Title flows into the <title> tag in base.html.
type Page struct {
	Title string
	Data  any
}

// SessionsPage is the data shape the sessions list template
// consumes. Keeping the rendering shape (this struct) separate
// from the storage shape (rows from store) lets us format
// timestamps, truncate prompts, and compute display-only fields
// (ShortID, LastActivity) once per request without pushing
// presentation logic into the SQL layer.
type SessionsPage struct {
	Title    string
	Sessions []SessionRow
}

// SessionRow is one row in the sessions list. Display strings
// are pre-rendered server-side so the template stays free of
// formatting helpers — easier to test and easier to keep tidy.
type SessionRow struct {
	ID           string // full UUID — used in /sessions/{id} hrefs
	ShortID      string // 8-char preview — what the user sees in the row
	LastActivity string // human-friendly relative time ("2h ago")
	EventCount   int
	Cwd          string // working directory at last event, or "-"
	FirstPrompt  string // truncated first user_prompt content_text
	HasSummary   bool   // an llm_outputs(kind='summary') row exists for this session
}
