package events

// events.EventView is the read-only shape used by code that walks stored
// events (prompt builders, export, audit). Nullable fields use
// events.NullString so consumers can distinguish "empty string"
// from "column was NULL".
//
// events.EventView is the public domain shape; the SQL-layer scan code
// in internal/store converts each row into an events.EventView at the
// query boundary so this type stays free of database/sql.
type EventView struct {
	EventID      string
	Kind         string
	Role         NullString
	ContentText  NullString
	TsSourceMs   int64
	ToolName     NullString
	SubagentID   NullString
	SubagentType NullString
	// Cwd is the working directory recorded on the event when the
	// hook captured it. NULL for events whose hook frame omitted
	// the cwd. Populated only by loaders that explicitly SELECT
	// events.cwd; legacy paths that don't need it leave the field
	// zero-valued.
	Cwd NullString
}
