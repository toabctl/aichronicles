package events

// Extraction is one structured fact derived from an envelope. The
// caller writes it to wherever extractions are persisted (SQLite's
// extractions table in our case); Extra serialises to JSON for any
// per-fact metadata that doesn't fit in the (Kind, Value) pair.
type Extraction struct {
	Kind  string
	Value string
	Extra map[string]any
}

// Extractor is the uniform contract every extractor satisfies: pure
// (no state, no I/O), total (no error — best-effort), safe for
// concurrent use. Returns zero or more extractions.
//
// Whether an Extractor runs against a given Envelope is decided by
// where it's registered in an ExtractorRegistry, NOT by gating
// inside the function body. A Tool extractor can assume
// env.Tool.Name matches its key by the time it's called; the
// registry is responsible for that dispatch.
type Extractor func(*Envelope) []Extraction

// Extraction kind names — the closed vocabulary stored in
// extractions.kind. Kept as exported constants so SQL queries and
// MCP tools can reference them by symbol.
const (
	ExtractionKindURL          = "url"
	ExtractionKindFilePath     = "file_path"
	ExtractionKindShellCommand = "shell_command"
	ExtractionKindSkillLoad    = "skill_load"
)

// ExtractorRegistry routes envelopes to extractors. The registry IS
// the dispatch table — reading DefaultExtractors() tells you the
// full mapping of "what runs for what" without needing to scan
// individual extractor bodies for their guards.
//
//   - Content extractors run on every envelope. Use when the
//     fact-of-interest lives in unstructured text the agent didn't
//     pre-structure for us (URLs in prose).
//
//   - Tool extractors run only when env.Tool != nil and
//     env.Tool.Name matches the map key. The function body can
//     trust the tool name is correct.
type ExtractorRegistry struct {
	Content []Extractor
	Tool    map[string][]Extractor
}

// Run walks the registry and returns every extraction the
// applicable extractors emit. Order is: every Content extractor in
// slice order, then every Tool extractor for the matching tool in
// slice order. Stable so integration tests asserting on row order
// stay reliable.
func (r *ExtractorRegistry) Run(env *Envelope) []Extraction {
	if env == nil || r == nil {
		return nil
	}
	var out []Extraction
	for _, e := range r.Content {
		out = append(out, e(env)...)
	}
	if env.Tool != nil {
		for _, e := range r.Tool[env.Tool.Name] {
			out = append(out, e(env)...)
		}
	}
	return out
}

// defaultRegistry is the package-level production wiring. Cached
// because the registry is immutable in practice and IngestEnvelope-
// equivalent callers run hot. Tests that need a different shape
// build their own ExtractorRegistry value rather than mutating this
// one.
var defaultRegistry = &ExtractorRegistry{
	Content: []Extractor{URLExtractor},
	Tool: map[string][]Extractor{
		"Bash":         {ShellCommandExtractor},
		"Read":         {FilePathExtractor},
		"Write":        {FilePathExtractor},
		"Edit":         {FilePathExtractor},
		"NotebookEdit": {FilePathExtractor},
		"WebFetch":     {WebFetchExtractor},
		"Skill":        {SkillLoadExtractor},
	},
}

// DefaultExtractors returns the production registry. The full
// dispatch table — what extractor runs for what tool — is in the
// var initializer above so a new reader can audit it at a glance.
//
// Adding a tool: add an entry to the Tool map (plus a new extractor
// function in extractors_builtin.go if the field-mapping is
// non-trivial). Removing a tool: delete the entry; the extractor
// function body, if shared with another tool, stays.
func DefaultExtractors() *ExtractorRegistry {
	return defaultRegistry
}
