# aichronicles build helpers.
#
# Production code is built with plain `go build`/`go install`; this
# Makefile only carries developer-facing helpers (doc regeneration,
# drift checks). Targets are intentionally short — when a step grows
# beyond two commands, promote it to a Go program under tools/.

.PHONY: docs docs-cli docs-schema docs-detectors docs-check

# Regenerate every auto-generated reference page. Run after editing
# command Long strings, schema migrations, or the redaction detector
# list. Idempotent: re-running on unchanged inputs produces byte-
# identical output.
docs: docs-cli docs-schema docs-detectors

# Per-subcommand markdown via cobra.GenMarkdownTreeCustom. Sources
# the live `cobra.Command.Long` strings, so editing flag help in
# internal/cli/*.go updates the docs.
docs-cli:
	go run ./tools/docgen cli

# SQL schema reference assembled from the embedded migrations FS,
# in apply order.
docs-schema:
	go run ./tools/docgen schema

# Redaction detector reference enumerated from
# redact.BuiltinDetectors(). New detectors land here automatically
# once the source ships them.
docs-detectors:
	go run ./tools/docgen detectors

# CI-friendly drift check: regenerate to a temp tree, diff against
# what's committed, fail if anything moved. Add to CI to keep the
# generated reference honest with the source of truth.
docs-check:
	@tmp=$$(mktemp -d) && trap "rm -rf $$tmp" EXIT && \
		cp -r docs/reference $$tmp/before && \
		$(MAKE) -s docs && \
		diff -ruN $$tmp/before docs/reference || \
		(echo "docs are stale — run 'make docs' and commit the result" && exit 1)
