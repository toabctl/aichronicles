package events

// Episode is one bounded, contextually-coherent run of events
// within a session — Pink et al. (2026 — arXiv:2502.06975) frame
// it as the episodic-memory unit agents need to retrieve concrete
// prior trajectories. Lives in internal/events as a domain type so
// consumers (prompt builders, web rendering, MCP find_episodes)
// import it from the domain package rather than internal/store.
//
// ID is a SQL rowid in the SQLite implementation; non-SQL Sinks
// would assign their own. EventCount is the number of stored
// events bracketed by [StartedAtMs, EndedAtMs] for this session.
type Episode struct {
	ID            int64
	SessionID     string
	Ordinal       int
	StartedAtMs   int64
	EndedAtMs     int64
	Cwd           NullString
	IntentSummary string
	EventCount    int
	FirstEventID  string
}
