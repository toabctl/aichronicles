//go:build e2e

package web_e2e_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// TestE2E_PageLoadsWithoutConsoleErrors is the smoke test. If the
// htmx wiring on the sessions page emits a console error (missing
// extension, malformed selector, dangling sse-swap, etc.) the rest
// of the e2e suite is meaningless — fail loudly and early.
func TestE2E_PageLoadsWithoutConsoleErrors(t *testing.T) {
	env := startEnv(t)
	defer env.Stop()
	// Seed one session so the table actually renders rows; an empty
	// store would short-circuit past the SSE-bearing markup.
	env.ingestEvent(t, "user_prompt", "smoke-1", "smoke probe", time.Now().Add(-30*time.Second))

	browserCtx, _ := newBrowser(t)
	pageCtx, pageCancel := chromedp.NewContext(browserCtx)
	defer pageCancel()

	var consoleMessages []string
	chromedp.ListenTarget(pageCtx, func(ev any) {
		if e, ok := ev.(*runtime.EventConsoleAPICalled); ok {
			if e.Type == "error" {
				var parts []string
				for _, arg := range e.Args {
					if arg.Value != nil {
						parts = append(parts, string(arg.Value))
					}
				}
				consoleMessages = append(consoleMessages,
					strings.Join(parts, " "))
			}
		}
	})

	ctx, cancel := withTimeout(pageCtx, 10*time.Second)
	defer cancel()

	if err := chromedp.Run(ctx,
		chromedp.Navigate(env.BaseURL+"/"),
		chromedp.WaitVisible(`table.sessions`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("load /: %v", err)
	}

	// htmx + the SSE extension take a beat to install listeners.
	// Give them a chance to error if they're going to.
	time.Sleep(500 * time.Millisecond)

	if len(consoleMessages) > 0 {
		t.Fatalf("page emitted %d console errors:\n%s",
			len(consoleMessages), strings.Join(consoleMessages, "\n"))
	}
}

// TestE2E_LiveFeedReceivesIngestedEvent is the headline test for
// the live feed: a fresh event ingested AFTER the page loads must
// arrive in the <ul id="livefeed"> via SSE without a page refresh.
// This is the path the user reported as broken — if it fails, the
// failure pinpoints the SSE → htmx → DOM hop.
func TestE2E_LiveFeedReceivesIngestedEvent(t *testing.T) {
	env := startEnv(t)
	defer env.Stop()

	browserCtx, _ := newBrowser(t)
	pageCtx, pageCancel := chromedp.NewContext(browserCtx)
	defer pageCancel()

	loadCtx, loadCancel := withTimeout(pageCtx, 10*time.Second)
	defer loadCancel()
	if err := chromedp.Run(loadCtx,
		chromedp.Navigate(env.BaseURL+"/"),
		chromedp.WaitVisible(`#livefeed`, chromedp.ByID),
	); err != nil {
		t.Fatalf("load: %v", err)
	}

	// Settle so the SSE EventSource is connected and the handler
	// has installed its cursor at MAX(ingest_seq).
	time.Sleep(400 * time.Millisecond)

	// Now ingest — this happens AFTER the page is open, so the
	// SSE connection must deliver it.
	env.ingestEvent(t, "user_prompt", "live-feed-test",
		"unique-marker-feed-tasty-orange", time.Now())

	// Wait for the marker text to appear inside #livefeed.
	pollUntil(t, 5*time.Second, func() error {
		var html string
		ctx, cancel := withTimeout(pageCtx, 1*time.Second)
		defer cancel()
		if err := chromedp.Run(ctx,
			chromedp.OuterHTML(`#livefeed`, &html, chromedp.ByID),
		); err != nil {
			return fmt.Errorf("get livefeed: %w", err)
		}
		if !strings.Contains(html, "unique-marker-feed-tasty-orange") {
			return fmt.Errorf("livefeed has not received the event yet:\n%s", html)
		}
		// The "(waiting for new events…)" placeholder must NOT be
		// the only content — a livefeed-row should be present.
		if !strings.Contains(html, "livefeed-row") {
			return fmt.Errorf("livefeed received content but no <li class=livefeed-row>:\n%s", html)
		}
		return nil
	})
}

// TestE2E_PerSessionLatestCellUpdatesLive verifies the column we
// just shipped: each row's "Latest event" <td> swaps in fresh
// content as new events for THAT session arrive on the SSE stream.
//
// Setup ingests one event up front so the row exists in the page;
// then a second event drives the cell update. Two events also tests
// that the cell reflects the LATEST one, not the first.
func TestE2E_PerSessionLatestCellUpdatesLive(t *testing.T) {
	env := startEnv(t)
	defer env.Stop()

	// Seed an initial event so the session row exists at page-load.
	sessionID := env.ingestEvent(t, "user_prompt", "row-update-test",
		"initial prompt — pre-load",
		time.Now().Add(-10*time.Second))

	browserCtx, _ := newBrowser(t)
	pageCtx, pageCancel := chromedp.NewContext(browserCtx)
	defer pageCancel()

	loadCtx, loadCancel := withTimeout(pageCtx, 10*time.Second)
	defer loadCancel()
	cellSelector := fmt.Sprintf(`td.latest[sse-swap="session-%s"]`, sessionID)
	if err := chromedp.Run(loadCtx,
		chromedp.Navigate(env.BaseURL+"/"),
		chromedp.WaitVisible(cellSelector, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("load: %v", err)
	}

	// Initial cell content reflects the first event.
	var initialHTML string
	if err := chromedp.Run(pageCtx,
		chromedp.OuterHTML(cellSelector, &initialHTML, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("initial OuterHTML: %v", err)
	}
	if !strings.Contains(initialHTML, "initial prompt") {
		t.Fatalf("server-side render missing initial event text:\n%s", initialHTML)
	}

	// Settle, then ingest a SECOND event for the same session.
	time.Sleep(400 * time.Millisecond)
	env.ingestEvent(t, "tool_use", "row-update-test",
		"second-event-marker-purple-llama", time.Now())

	pollUntil(t, 5*time.Second, func() error {
		var html string
		ctx, cancel := withTimeout(pageCtx, 1*time.Second)
		defer cancel()
		if err := chromedp.Run(ctx,
			chromedp.OuterHTML(cellSelector, &html, chromedp.ByQuery),
		); err != nil {
			return fmt.Errorf("get cell: %w", err)
		}
		if !strings.Contains(html, "second-event-marker-purple-llama") {
			return fmt.Errorf("cell did not pick up the second event:\n%s", html)
		}
		// And the new badge should be the new kind.
		if !strings.Contains(html, "tool_use") {
			return fmt.Errorf("cell did not pick up the new kind badge:\n%s", html)
		}
		return nil
	})
}

// TestE2E_StatusDotFlipsToEndedOnSessionEnd verifies the OOB
// status-dot swap. A session that was active at page-load must
// have its dot flip to status-ended once a session_end event
// lands on the SSE stream. This validates the OOB swap: the SSE
// frame's main payload swaps the latest-event cell, but the
// hx-swap-oob span on the status dot is processed independently.
func TestE2E_StatusDotFlipsToEndedOnSessionEnd(t *testing.T) {
	env := startEnv(t)
	defer env.Stop()

	sessionID := env.ingestEvent(t, "user_prompt", "ending-test",
		"working away", time.Now().Add(-30*time.Second))

	browserCtx, _ := newBrowser(t)
	pageCtx, pageCancel := chromedp.NewContext(browserCtx)
	defer pageCancel()

	dotSelector := fmt.Sprintf(`#status-%s`, sessionID)
	loadCtx, loadCancel := withTimeout(pageCtx, 10*time.Second)
	defer loadCancel()
	if err := chromedp.Run(loadCtx,
		chromedp.Navigate(env.BaseURL+"/"),
		chromedp.WaitVisible(dotSelector, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("load: %v", err)
	}

	// At page-load the recently-active session should have status-active.
	var initialClass string
	if err := chromedp.Run(pageCtx,
		chromedp.AttributeValue(dotSelector, "class", &initialClass, nil, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("initial class: %v", err)
	}
	if !strings.Contains(initialClass, "status-active") {
		t.Fatalf("expected status-active at load, got %q", initialClass)
	}

	time.Sleep(400 * time.Millisecond)
	env.ingestEvent(t, "session_end", "ending-test", "", time.Now())

	pollUntil(t, 5*time.Second, func() error {
		var class string
		ctx, cancel := withTimeout(pageCtx, 1*time.Second)
		defer cancel()
		var ok bool
		if err := chromedp.Run(ctx,
			chromedp.AttributeValue(dotSelector, "class", &class, &ok, chromedp.ByQuery),
		); err != nil {
			return fmt.Errorf("get class: %w", err)
		}
		if !ok {
			return fmt.Errorf("status dot %s no longer present", dotSelector)
		}
		if !strings.Contains(class, "status-ended") {
			return fmt.Errorf("status dot still %q, want status-ended", class)
		}
		return nil
	})
}

// TestE2E_SessionDetailLiveBannerReceivesEvents covers the third
// SSE-bearing surface: the per-session banner on /sessions/{id}.
// New events for that session must land in the banner; events for
// other sessions must not.
func TestE2E_SessionDetailLiveBannerReceivesEvents(t *testing.T) {
	env := startEnv(t)
	defer env.Stop()

	target := env.ingestEvent(t, "user_prompt", "banner-test", "anything",
		time.Now().Add(-5*time.Second))
	// A second session whose events MUST NOT appear on this detail page.
	other := env.ingestEvent(t, "user_prompt", "other-session", "ignore me",
		time.Now().Add(-5*time.Second))
	_ = other

	browserCtx, _ := newBrowser(t)
	pageCtx, pageCancel := chromedp.NewContext(browserCtx)
	defer pageCancel()

	loadCtx, loadCancel := withTimeout(pageCtx, 10*time.Second)
	defer loadCancel()
	if err := chromedp.Run(loadCtx,
		chromedp.Navigate(env.BaseURL+"/sessions/"+target),
		chromedp.WaitVisible(`#livebanner-target`, chromedp.ByID),
	); err != nil {
		t.Fatalf("load detail: %v", err)
	}
	time.Sleep(400 * time.Millisecond)

	// Ingest an event for the OTHER session first — the banner
	// must NOT show it (filter enforced server-side via ?session_id).
	env.ingestEvent(t, "user_prompt", "other-session",
		"distractor-event-marker-cyan-gnu", time.Now())

	// Then ingest one for the target session that the banner SHOULD show.
	env.ingestEvent(t, "user_prompt", "banner-test",
		"target-event-marker-amber-yak", time.Now())

	pollUntil(t, 5*time.Second, func() error {
		var html string
		ctx, cancel := withTimeout(pageCtx, 1*time.Second)
		defer cancel()
		if err := chromedp.Run(ctx,
			chromedp.OuterHTML(`#livebanner-target`, &html, chromedp.ByID),
		); err != nil {
			return fmt.Errorf("get banner: %w", err)
		}
		if !strings.Contains(html, "target-event-marker-amber-yak") {
			return fmt.Errorf("banner missing target session event:\n%s", html)
		}
		if strings.Contains(html, "distractor-event-marker-cyan-gnu") {
			return fmt.Errorf("session_id filter leaked: banner shows other session's event:\n%s", html)
		}
		return nil
	})
}
