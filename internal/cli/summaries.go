package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/toabctl/aichronicles/internal/paths"
	"github.com/toabctl/aichronicles/internal/store"
)

// newSummariesCmd is the `summaries` subcommand tree. `list` gives
// a scannable recent history across all kinds; `show` prints one
// stored body through the human renderer (or raw via --json).
func newSummariesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "summaries",
		Short: "Inspect stored LLM outputs (summaries, reflections, proposals)",
	}
	cmd.AddCommand(newSummariesListCmd())
	cmd.AddCommand(newSummariesShowCmd())
	return cmd
}

func newSummariesListCmd() *cobra.Command {
	var (
		sessionIn string
		kindIn    string
		limit     int
		dbPath    string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List recent stored LLM outputs",
		Long: "Prints stored llm_outputs rows newest-first. Without flags, it\n" +
			"shows the latest 50 across every session and every kind. Filter\n" +
			"with --session (prefix OK, same rules as `summarize`), --kind\n" +
			"(summary | reflect | propose), or both.\n\n" +
			"Topic column is extracted from the stored JSON body when\n" +
			"possible; rows whose body is not parseable as a known kind\n" +
			"show `(unparseable)` so the row is still discoverable by id.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := openStoreFromFlag(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			filter := store.LLMOutputFilter{Limit: limit}
			if sessionIn != "" {
				sid, err := store.ResolveSessionIDPrefix(cmd.Context(), s.DB(), sessionIn)
				if err != nil {
					return fmt.Errorf("summaries list: %w", err)
				}
				filter.SessionID = sid
			}
			if kindIn != "" {
				k, err := parseOutputKind(kindIn)
				if err != nil {
					return err
				}
				filter.Kind = k
			}

			rows, err := store.LoadLLMOutputs(cmd.Context(), s.DB(), filter)
			if err != nil {
				return fmt.Errorf("summaries list: %w", err)
			}
			return writeSummariesTable(cmd.OutOrStdout(), rows)
		},
	}
	cmd.Flags().StringVar(&sessionIn, "session", "", "filter by session id or unique prefix")
	cmd.Flags().StringVar(&kindIn, "kind", "", "filter by kind (summary | reflect | propose)")
	cmd.Flags().IntVar(&limit, "limit", 0, "max rows to list (default 50)")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (default: $XDG_STATE_HOME/aichronicles/store.db)")
	return cmd
}

func newSummariesShowCmd() *cobra.Command {
	var (
		kindIn  string
		jsonOut bool
		dbPath  string
	)
	cmd := &cobra.Command{
		Use:   "show <session>",
		Short: "Show the most recent stored LLM output for a session",
		Long: "Renders the latest llm_outputs row matching the given session\n" +
			"(prefix OK) and kind (default: summary). Pass --json to emit the\n" +
			"raw JSON body instead of the human-readable render — useful for\n" +
			"piping into `jq`.\n\n" +
			"Errors with `no output for session …/kind …` when the session\n" +
			"exists but has never been summarized/reflected/proposed under\n" +
			"the requested kind.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openStoreFromFlag(dbPath)
			if err != nil {
				return err
			}
			defer func() { _ = s.Close() }()

			sid, err := store.ResolveSessionIDPrefix(cmd.Context(), s.DB(), args[0])
			if err != nil {
				return fmt.Errorf("summaries show: %w", err)
			}
			kind := store.LLMKindSummary
			if kindIn != "" {
				k, err := parseOutputKind(kindIn)
				if err != nil {
					return err
				}
				kind = k
			}

			rows, err := store.LoadLLMOutputs(cmd.Context(), s.DB(), store.LLMOutputFilter{
				SessionID: sid,
				Kind:      kind,
				Limit:     1,
			})
			if err != nil {
				return fmt.Errorf("summaries show: %w", err)
			}
			if len(rows) == 0 {
				return fmt.Errorf("no %s output for session %s", kind, sid)
			}
			return emitLLMBody(cmd.OutOrStdout(), kind, rows[0].Body, jsonOut)
		},
	}
	cmd.Flags().StringVar(&kindIn, "kind", "", "output kind (summary | reflect | propose; default: summary)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit raw JSON body instead of the human-readable render")
	cmd.Flags().StringVar(&dbPath, "db", "", "SQLite DB path (default: $XDG_STATE_HOME/aichronicles/store.db)")
	return cmd
}

// openStoreFromFlag factors the dbPath-or-default resolution every
// subcommand in this file repeats. Callers defer s.Close().
func openStoreFromFlag(dbPath string) (*store.Store, error) {
	resolved := dbPath
	if resolved == "" {
		p, err := paths.StorePath()
		if err != nil {
			return nil, err
		}
		resolved = p
	}
	return store.Open(resolved)
}

// parseOutputKind normalizes the --kind flag into a store.LLMOutputKind.
// Accepting the short forms the CLI prints in its listing rather than
// forcing users to type the full lowercase string.
func parseOutputKind(s string) (store.LLMOutputKind, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "summary":
		return store.LLMKindSummary, nil
	case "reflect", "reflection":
		return store.LLMKindReflect, nil
	case "propose", "proposal":
		return store.LLMKindPropose, nil
	default:
		return "", fmt.Errorf("unknown kind %q (want summary | reflect | propose)", s)
	}
}

// writeSummariesTable renders a tab-aligned table of the rows. Empty
// result set produces a single "(no outputs)" line so the user sees
// that the command ran but found nothing.
func writeSummariesTable(w io.Writer, rows []store.LLMOutput) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "(no outputs)")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tKIND\tSESSION\tWHEN\tTOPIC"); err != nil {
		return err
	}
	for _, r := range rows {
		topic := extractTopic(r.Kind, r.Body)
		sess := "(multi)"
		if r.SessionID.Valid && r.SessionID.String != "" {
			sess = shortSessionID(r.SessionID.String)
		}
		when := time.UnixMilli(r.CreatedAtMs).UTC().Format("2006-01-02 15:04")
		if _, err := fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n",
			r.ID, r.Kind, sess, when, topic); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// shortSessionID returns the first 8 chars of a full UUID — matches
// the preview `aichronicles sessions` prints, so column alignment
// across commands stays consistent.
func shortSessionID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// extractTopic picks a short, human-meaningful label out of a stored
// body based on kind. Falls back to "(unparseable)" when the body
// doesn't fit the expected schema — survival mode for legacy rows or
// post-schema-bump drift.
func extractTopic(kind store.LLMOutputKind, body string) string {
	const maxLen = 80
	truncate := func(s string) string {
		s = strings.ReplaceAll(s, "\n", " ")
		if len(s) > maxLen {
			return s[:maxLen] + "…"
		}
		return s
	}
	switch kind {
	case store.LLMKindSummary:
		var r struct {
			Topic string `json:"topic"`
		}
		if err := json.Unmarshal([]byte(body), &r); err != nil || r.Topic == "" {
			return "(unparseable)"
		}
		return truncate(r.Topic)
	case store.LLMKindReflect:
		var r struct {
			WorkflowChange string `json:"workflow_change"`
		}
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			return "(unparseable)"
		}
		if r.WorkflowChange == "" {
			return "(no workflow change suggested)"
		}
		return truncate(r.WorkflowChange)
	case store.LLMKindPropose:
		var r struct {
			Skills []struct {
				Name string `json:"name"`
			} `json:"skills"`
		}
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			return "(unparseable)"
		}
		switch len(r.Skills) {
		case 0:
			return "(no proposals)"
		case 1:
			return truncate(r.Skills[0].Name)
		default:
			// First skill + "(+ N more)" so the row still fits.
			return truncate(fmt.Sprintf("%s (+%d more)", r.Skills[0].Name, len(r.Skills)-1))
		}
	}
	return "(unknown kind)"
}
