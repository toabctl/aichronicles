package cli

import (
	"context"
	"log/slog"

	"github.com/toabctl/aichronicles/internal/apiclient"
	"github.com/toabctl/aichronicles/internal/llm/prompts"
)

// loadSkillsEnrichment fetches the installed- and invoked-skill
// context the propose / induce / verify prompts are enriched with.
//
// Goes through the daemon rather than opening SQLite directly. The
// CLI used to call skills.CollectInstalled / skills.LoadInvoked with
// a raw *sql.DB, and those four call sites were the entire remaining
// reason `openStore` and `--db` existed across nine commands — each
// one a second writer connection against the database the daemon
// already owns. The endpoints, wire types and apiclient methods have
// existed and been exercised by internal/web and internal/mcp all
// along; only the CLI reached around them.
//
// Errors are non-fatal by design and match the previous behaviour:
// enrichment makes the prompt better, so losing it degrades quality
// rather than failing the command. The caller logs and continues with
// whatever came back.
func loadSkillsEnrichment(
	ctx context.Context,
	c *apiclient.Client,
	sinceMs int64,
	logPrefix string,
) (installed []prompts.InstalledSkill, invoked []prompts.InvokedSkill) {
	if resp, err := c.InstalledSkills(ctx, sinceMs); err != nil {
		slog.Warn(logPrefix+": skipping installed-skills enrichment", "err", err)
	} else {
		installed = make([]prompts.InstalledSkill, 0, len(resp.Skills))
		for _, s := range resp.Skills {
			installed = append(installed, prompts.InstalledSkill{
				Name:        s.Name,
				Description: s.Description,
				Source:      s.Source,
			})
		}
	}

	if resp, err := c.InvokedSkills(ctx, sinceMs); err != nil {
		slog.Warn(logPrefix+": skipping invoked-skills enrichment", "err", err)
	} else {
		invoked = make([]prompts.InvokedSkill, 0, len(resp.Skills))
		for _, s := range resp.Skills {
			invoked = append(invoked, prompts.InvokedSkill{
				Name:  s.Name,
				Count: s.Count,
			})
		}
	}
	return installed, invoked
}

// loadInstalledSkills is loadSkillsEnrichment for the callers that
// need only the installed half.
func loadInstalledSkills(
	ctx context.Context,
	c *apiclient.Client,
	sinceMs int64,
	logPrefix string,
) []prompts.InstalledSkill {
	installed, _ := loadSkillsEnrichment(ctx, c, sinceMs, logPrefix)
	return installed
}
