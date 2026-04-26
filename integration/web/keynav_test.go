//go:build e2e

package web_e2e_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

// TestE2E_KeynavArrowDownFocusesFirstRow drives the headline keynav
// path: a fresh page with no row focused, ArrowDown should land on
// the first .nav-row and give it the visible focus ring.
func TestE2E_KeynavArrowDownFocusesFirstRow(t *testing.T) {
	env := startEnv(t)
	defer env.Stop()

	// Two sessions so we can also test that a second ArrowDown
	// moves to the second row, not just sticks on the first.
	env.ingestEvent(t, "user_prompt", "keynav-1", "first session",
		time.Now().Add(-2*time.Minute))
	env.ingestEvent(t, "user_prompt", "keynav-2", "second session",
		time.Now().Add(-1*time.Minute))

	browserCtx, _ := newBrowser(t)
	pageCtx, pageCancel := chromedp.NewContext(browserCtx)
	defer pageCancel()

	loadCtx, loadCancel := withTimeout(pageCtx, 10*time.Second)
	defer loadCancel()
	if err := chromedp.Run(loadCtx,
		chromedp.Navigate(env.BaseURL+"/"),
		chromedp.WaitVisible(`tr.nav-row`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("load: %v", err)
	}

	// Click on body to make sure no input has focus, then press
	// ArrowDown twice. Final activeElement should be the SECOND
	// row's index in the nav-row list.
	var activeIndex int
	if err := chromedp.Run(pageCtx,
		chromedp.Click("body", chromedp.ByQuery),
		chromedp.KeyEvent(kb.ArrowDown),
		chromedp.KeyEvent(kb.ArrowDown),
		chromedp.Evaluate(`(function () {
			const rows = document.querySelectorAll('tr.nav-row');
			return Array.prototype.indexOf.call(rows, document.activeElement);
		})()`, &activeIndex),
	); err != nil {
		t.Fatalf("keynav: %v", err)
	}
	if activeIndex != 1 {
		t.Errorf("after two ArrowDowns expected row index 1 (second row), got %d", activeIndex)
	}
}

// TestE2E_KeynavArrowUpClamps verifies the wrap behaviour: the
// first row holds focus when ArrowUp is pressed past it. Prevents
// a regression that would silently lose focus off the top of the
// table.
func TestE2E_KeynavArrowUpClamps(t *testing.T) {
	env := startEnv(t)
	defer env.Stop()

	env.ingestEvent(t, "user_prompt", "keynav-clamp", "only one row",
		time.Now().Add(-30*time.Second))

	browserCtx, _ := newBrowser(t)
	pageCtx, pageCancel := chromedp.NewContext(browserCtx)
	defer pageCancel()

	loadCtx, loadCancel := withTimeout(pageCtx, 10*time.Second)
	defer loadCancel()
	if err := chromedp.Run(loadCtx,
		chromedp.Navigate(env.BaseURL+"/"),
		chromedp.WaitVisible(`tr.nav-row`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("load: %v", err)
	}

	var idx int
	if err := chromedp.Run(pageCtx,
		chromedp.Click("body", chromedp.ByQuery),
		chromedp.KeyEvent(kb.ArrowDown), // → row 0
		chromedp.KeyEvent(kb.ArrowUp),   // would go to -1 but clamps at 0
		chromedp.KeyEvent(kb.ArrowUp),   // still 0
		chromedp.Evaluate(`(function () {
			const rows = document.querySelectorAll('tr.nav-row');
			return Array.prototype.indexOf.call(rows, document.activeElement);
		})()`, &idx),
	); err != nil {
		t.Fatalf("keynav: %v", err)
	}
	if idx != 0 {
		t.Errorf("ArrowUp past the top should clamp at row 0, got %d", idx)
	}
}

// TestE2E_KeynavEnterNavigatesToSession verifies the Enter
// behaviour: focused row + Enter clicks the row's first <a> and
// the page navigates to the session detail.
func TestE2E_KeynavEnterNavigatesToSession(t *testing.T) {
	env := startEnv(t)
	defer env.Stop()

	target := env.ingestEvent(t, "user_prompt", "keynav-enter",
		"navigate to me", time.Now().Add(-30*time.Second))

	browserCtx, _ := newBrowser(t)
	pageCtx, pageCancel := chromedp.NewContext(browserCtx)
	defer pageCancel()

	loadCtx, loadCancel := withTimeout(pageCtx, 10*time.Second)
	defer loadCancel()
	if err := chromedp.Run(loadCtx,
		chromedp.Navigate(env.BaseURL+"/"),
		chromedp.WaitVisible(`tr.nav-row`, chromedp.ByQuery),
		chromedp.Click("body", chromedp.ByQuery),
		chromedp.KeyEvent(kb.ArrowDown),
		chromedp.KeyEvent(kb.Enter),
		chromedp.WaitVisible(`#livebanner-target`, chromedp.ByID),
	); err != nil {
		t.Fatalf("keynav enter: %v", err)
	}

	// Confirm we landed on the right session detail page.
	var url string
	if err := chromedp.Run(pageCtx,
		chromedp.Evaluate(`window.location.pathname`, &url),
	); err != nil {
		t.Fatalf("get url: %v", err)
	}
	want := "/sessions/" + target
	if !strings.Contains(url, want) {
		t.Errorf("Enter did not navigate to %s; landed on %s", want, url)
	}
}

// TestE2E_KeynavSlashFocusesNavSearch verifies the global
// "/" hotkey: outside any input, "/" pulls focus into the
// nav-bar search box.
func TestE2E_KeynavSlashFocusesNavSearch(t *testing.T) {
	env := startEnv(t)
	defer env.Stop()
	env.ingestEvent(t, "user_prompt", "keynav-slash", "anything",
		time.Now().Add(-30*time.Second))

	browserCtx, _ := newBrowser(t)
	pageCtx, pageCancel := chromedp.NewContext(browserCtx)
	defer pageCancel()

	loadCtx, loadCancel := withTimeout(pageCtx, 10*time.Second)
	defer loadCancel()
	if err := chromedp.Run(loadCtx,
		chromedp.Navigate(env.BaseURL+"/"),
		chromedp.WaitVisible(`.navsearch input[type=search]`, chromedp.ByQuery),
	); err != nil {
		t.Fatalf("load: %v", err)
	}

	var focusedTag string
	if err := chromedp.Run(pageCtx,
		chromedp.Click("body", chromedp.ByQuery),
		chromedp.KeyEvent("/"),
		chromedp.Evaluate(`(function() {
			const a = document.activeElement;
			return a ? a.tagName + (a.type ? ":" + a.type : "") : "";
		})()`, &focusedTag),
	); err != nil {
		t.Fatalf("keynav slash: %v", err)
	}
	if focusedTag != "INPUT:search" {
		t.Errorf("after pressing '/', expected the search input to be focused; got %q",
			focusedTag)
	}

	_ = fmt.Sprintf // keep fmt referenced under future debug-print edits
}
