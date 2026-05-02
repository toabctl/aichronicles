# aichronicles build helpers.
#
# Targets are intentionally short — when a step grows beyond two
# commands, promote it to a Go program under tools/.

# Where binaries land. ~/.local/bin matches the path the systemd unit
# expects (assets/aichronicles-api.service ExecStart=%h/.local/bin/...).
PREFIX ?= $(HOME)/.local
BINDIR := $(PREFIX)/bin

# Extra flags passed to every `go build`. Override on the command line
# (e.g. `make GOFLAGS=-trimpath build`) when reproducible builds matter.
GOFLAGS ?=

# Version stamp injected via `-ldflags -X`. Falls back to "dev" when
# the tree has no tags or git is unavailable so `make build` still
# produces a working binary in a fresh checkout. The result lands in
# `cli.Version` and surfaces via --version on both binaries.
GIT_DESCRIBE := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS ?= -X 'github.com/toabctl/aichronicles/internal/cli.Version=$(GIT_DESCRIBE)'

# Default target: build the two binaries AND install them. Surprising
# for a bare `make`, but the user asked for it explicitly so they can
# rebuild + restart in one keystroke.
.DEFAULT_GOAL := all

.PHONY: all build install clean docs docs-cli docs-schema docs-detectors docs-check depcheck

# Build then install. Restart of the systemd --user service is part
# of `install`, so `make` end-to-end gets the running daemon onto the
# new binary.
all: build install

# Compile both binaries into ./bin. Cleans only what it produces, so
# running it on a tree with uncommitted changes is safe.
build:
	@mkdir -p ./bin
	go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o ./bin/aichronicles      ./cmd/aichronicles
	go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o ./bin/aichronicles-api  ./cmd/aichronicles-api

# Copy both binaries into $(BINDIR) (default ~/.local/bin) and bounce
# the systemd --user service so the running daemon picks up the new
# code. The bounce is a no-op when the socket isn't installed yet
# (first-time install) — we just print a one-liner pointing at
# `aichronicles setup systemd`.
install: build
	@install -d $(BINDIR)
	install -m 0755 ./bin/aichronicles      $(BINDIR)/aichronicles
	install -m 0755 ./bin/aichronicles-api  $(BINDIR)/aichronicles-api
	@if systemctl --user is-enabled aichronicles-api.socket >/dev/null 2>&1; then \
	  echo "restarting aichronicles-api.service (systemd --user)"; \
	  systemctl --user restart aichronicles-api.service; \
	else \
	  echo "aichronicles-api.socket not installed — skipping restart."; \
	  echo "  run \`aichronicles setup systemd\` to wire up the socket."; \
	fi
	@# `try-restart` only bounces aichronicles-web.service when it's
	@# already running — the web service is socket-activated and may
	@# legitimately be dead between bursts; we don't want to start it
	@# here. When it IS running, the bounce makes the next request
	@# pick up the new binary instead of waiting for idle-shutdown.
	@if systemctl --user is-active aichronicles-web.service >/dev/null 2>&1; then \
	  echo "restarting aichronicles-web.service (was running)"; \
	  systemctl --user try-restart aichronicles-web.service; \
	fi

# Remove only what build/install produces locally.
clean:
	rm -rf ./bin


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

# Dependency-direction guard. Verifies the layering invariants
# the architecture relies on (pkg/api has no SQL/HTTP imports;
# internal/apiclient does not reach into internal/store; etc).
# CI should run this on every PR before review.
depcheck:
	go run ./tools/depcheck
