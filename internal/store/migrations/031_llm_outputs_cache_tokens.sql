-- Record Anthropic prompt-cache token counts alongside the base
-- input/output counts.
--
-- Every request marks its system prompt cacheable, and those prompts
-- are the large constants (proposeSystem and verifyProposalSystem are
-- ~4 KB each). Anthropic reports cache_creation_input_tokens and
-- cache_read_input_tokens SEPARATELY from input_tokens, and the
-- adapter mapped neither — so llm_outputs.input_tokens recorded only
-- the uncached remainder.
--
-- For a call like `propose verify`, a few hundred user tokens against
-- a 4 KB cached system block, that is an undercount by a large
-- multiple. It propagated to `aichronicles usage` and to every cost
-- figure derived from these rows.
--
-- Two columns rather than one because the three classes bill at
-- different rates: a cache write costs 1.25x the base input rate, a
-- read 0.1x. Summing them into input_tokens would fix the count and
-- break the cost.
--
-- Nullable with no default: NULL means "written before this column
-- existed, genuinely unknown", which is distinct from a real 0 on a
-- call that used no cache. Backfilling zeros would erase that.

ALTER TABLE llm_outputs ADD COLUMN cache_write_tokens INTEGER;
ALTER TABLE llm_outputs ADD COLUMN cache_read_tokens  INTEGER;

UPDATE meta SET value='31' WHERE key='schema_version';
