package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/toabctl/aichronicles/internal/events"
	"github.com/toabctl/aichronicles/internal/preview"
)

// resumePreviewPaneMessages is how many trailing messages the TUI's
// preview pane fetches per session. More than the old static cards
// since the pane has room to show context.
const resumePreviewPaneMessages = 8

// lipgloss styles for the picker. Package-level: lipgloss styles are
// immutable value builders, safe to share. When stdout is not a TTY
// (tests, pipes) lipgloss's renderer auto-detects and emits no colour,
// so View() stays plain and assertable.
var (
	resumeBoxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)
	resumeDimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	resumeTitleStyle  = lipgloss.NewStyle().Bold(true)
	resumeFooterStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).MarginTop(1)
	// Per-speaker styles colour the preview so your turns and the
	// agent's are instantly distinguishable: you in cyan, the agent in
	// green. The gutter bar carries the colour on every wrapped line so
	// each turn reads as one block.
	resumeYouStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("45")).Bold(true)
	resumeAsstStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)
	// List row styles: the selected row is cyan + bold with a ›
	// pointer, others are plain. One line per row (see resumeDelegate)
	// so short lists never paginate.
	resumeRowSelStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("45")).Bold(true)
)

// resumeDelegate renders each session as a single line — "short id ·
// when · cwd" — so the list fits many rows per page and a handful of
// sessions never triggers pagination. The selected row is highlighted
// with a › pointer; others are indented to align.
type resumeDelegate struct{}

func (resumeDelegate) Height() int                         { return 1 }
func (resumeDelegate) Spacing() int                        { return 0 }
func (resumeDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }
func (resumeDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	it, ok := item.(resumeItem)
	if !ok {
		return
	}
	line := it.title
	if it.desc != "" {
		line += "  " + it.desc
	}
	width := m.Width() - 2 // leave room for the marker
	if index == m.Index() {
		_, _ = fmt.Fprint(w, resumeRowSelStyle.Render("› "+truncateRunes(line, width)))
		return
	}
	_, _ = fmt.Fprint(w, "  "+truncateRunes(line, width))
}

// resumeItem is one row in the bubbles/list. It carries the candidate
// index so the model can map the list's selection back to m.cands
// regardless of how the list reorders internally.
type resumeItem struct {
	idx   int
	title string // short id · when
	desc  string // cwd
	fv    string // filter value (cwd + opening prompt)
}

func (i resumeItem) Title() string       { return i.title }
func (i resumeItem) Description() string { return i.desc }
func (i resumeItem) FilterValue() string { return i.fv }

// resumeModel is the bubbletea model behind the interactive picker: a
// bubbles/list of sessions on the left, a preview of the selected
// session on the right. chosen is the picked candidate index after the
// user hits enter, or -1 when they cancelled (q / esc / ctrl-c).
type resumeModel struct {
	cands    []resumeCandidate
	list     list.Model
	chosen   int
	width    int
	height   int
	quitting bool
}

func newResumeModel(cands []resumeCandidate) resumeModel {
	now := time.Now()
	items := make([]list.Item, len(cands))
	for i, c := range cands {
		d := c.digest
		items[i] = resumeItem{
			idx:   i,
			title: preview.ShortID(d.ID) + "  " + resumeWhen(d, now),
			desc:  strPtrOrDash(d.Cwd),
			fv:    strPtrOrDash(d.Cwd) + " " + strPtrOrDash(d.FirstPrompt),
		}
	}
	l := list.New(items, resumeDelegate{}, 0, 0)
	l.Title = "sessions"
	l.SetShowHelp(false)
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)
	// Default size lets View() render before the first WindowSizeMsg
	// (and in tests, which send their own size).
	leftW, _ := resumePaneWidths(96)
	l.SetSize(leftW, 18)
	return resumeModel{cands: cands, list: l, chosen: -1, width: 96, height: 24}
}

func (m resumeModel) Init() tea.Cmd { return nil }

func (m resumeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		leftW, _ := resumePaneWidths(msg.Width)
		listH := msg.Height - 4 // box border (2) + footer (~2)
		if listH < 3 {
			listH = 3
		}
		m.list.SetSize(leftW, listH)
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.quitting = true
			m.chosen = -1
			return m, tea.Quit
		case "enter":
			if it, ok := m.list.SelectedItem().(resumeItem); ok {
				m.chosen = it.idx
			}
			m.quitting = true
			return m, tea.Quit
		}
	}
	// All other keys (↑/↓, j/k, pgup/pgdn, home/end) drive the list,
	// which owns selection, scrolling, and pagination.
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m resumeModel) View() string {
	if m.quitting || len(m.cands) == 0 {
		return ""
	}
	_, rightW := resumePaneWidths(m.width)
	// Bound the preview body so the boxes + footer fit the screen: drop
	// the box borders (2), the pane's header lines (3), and the footer
	// (~2). Fill from the newest message backward so the latest exchange
	// is always visible.
	bodyLines := m.height - 2 - 3 - 2
	if bodyLines < 4 {
		bodyLines = 4
	}
	sel := m.list.Index()
	if sel < 0 || sel >= len(m.cands) {
		sel = 0
	}
	body := lipgloss.JoinHorizontal(
		lipgloss.Top,
		resumeBoxStyle.Render(m.list.View()),
		resumeBoxStyle.Width(rightW).Render(renderResumePreviewPane(m.cands[sel], rightW, bodyLines)),
	)
	footer := resumeFooterStyle.Render("↑/↓ move · enter resume · q quit")
	return body + "\n" + footer
}

// resumePaneWidths splits the terminal into a narrower list column and a
// wider preview column, leaving room for the two boxes' borders and
// padding so the joined layout fits within total columns.
func resumePaneWidths(total int) (left, right int) {
	if total < 50 {
		total = 50
	}
	avail := total - 10 // 2 boxes × (border 2 + padding 2) + slack
	left = avail * 2 / 5
	if left < 24 {
		left = 24
	}
	right = avail - left
	if right < 24 {
		right = 24
	}
	return left, right
}

// resumeMsgMaxLines caps how many wrapped lines a single message
// contributes to the preview, so one long turn can't crowd out the rest.
const resumeMsgMaxLines = 5

// renderResumePreviewPane renders the right column for the selected
// candidate: a compact header (cwd, when, opening prompt) followed by
// the trailing conversation. Each turn is a colour-coded block — a
// speaker-tinted gutter bar down its left edge, your turns in cyan and
// the agent's in green — wrapped to the pane width. Messages are filled
// newest-first up to maxBodyLines, then rendered chronologically so the
// latest exchange sits at the bottom and never scrolls off.
func renderResumePreviewPane(c resumeCandidate, width, maxBodyLines int) string {
	d := c.digest
	var head strings.Builder
	head.WriteString(resumeTitleStyle.Render(truncateRunes(strPtrOrDash(d.Cwd), width)) + "\n")
	when := resumeWhen(d, time.Now())
	if fp := strPtrOrDash(d.FirstPrompt); fp != "-" {
		when += "  ▸ " + flattenLine(fp)
	}
	head.WriteString(resumeDimStyle.Render(truncateRunes(when, width)) + "\n\n")

	if len(c.tail) == 0 {
		head.WriteString(resumeDimStyle.Render("(no message preview)"))
		return head.String()
	}
	if maxBodyLines < 4 {
		maxBodyLines = 4
	}

	// Build blocks newest→oldest until the budget is spent (always keep
	// at least the newest), then emit them oldest→newest.
	var blocks [][]string
	used := 0
	for i := len(c.tail) - 1; i >= 0; i-- {
		ev := c.tail[i]
		label, st := resumeSpeaker(ev.Kind, d.SourceAgent)
		bar := st.Render("▌")
		block := []string{bar + " " + st.Render(label)}
		for _, ln := range wrapWords(flattenLine(ptrStrOrEmpty(ev.ContentText)), width-2, resumeMsgMaxLines) {
			block = append(block, bar+" "+ln)
		}
		cost := len(block) + 1 // trailing blank between turns
		if used+cost > maxBodyLines && len(blocks) > 0 {
			break
		}
		blocks = append(blocks, block)
		used += cost
	}

	var body strings.Builder
	for bi := len(blocks) - 1; bi >= 0; bi-- {
		for _, ln := range blocks[bi] {
			body.WriteString(ln + "\n")
		}
		if bi > 0 {
			body.WriteString("\n")
		}
	}
	return head.String() + strings.TrimRight(body.String(), "\n")
}

// resumeSpeaker returns the display label and colour style for a
// message kind. The agent name (claude / gemini) is used for assistant
// turns so the preview reads naturally per source.
func resumeSpeaker(kind, agent string) (string, lipgloss.Style) {
	switch kind {
	case events.KindUserPrompt:
		return "you", resumeYouStyle
	case events.KindAssistantMessage:
		return resumeAgentName(agent), resumeAsstStyle
	default:
		return "·", resumeDimStyle
	}
}

// resumeAgentName maps a source_agent to the short label shown on
// assistant turns.
func resumeAgentName(agent string) string {
	switch agent {
	case "claude-code":
		return "claude"
	case "gemini-cli":
		return "gemini"
	default:
		return "assistant"
	}
}

// wrapWords soft-wraps s into lines of at most width runes, breaking on
// spaces (hard-splitting any single word longer than width), and caps
// the result at maxLines — the last kept line gets an ellipsis when
// content remains. Returns nil for non-positive bounds.
func wrapWords(s string, width, maxLines int) []string {
	if width <= 0 || maxLines <= 0 {
		return nil
	}
	var lines []string
	cur := ""
	push := func() { lines = append(lines, cur); cur = "" }
	for _, w := range strings.Fields(s) {
		if cur == "" {
			cur = w
		} else if len([]rune(cur))+1+len([]rune(w)) <= width {
			cur += " " + w
		} else {
			push()
			cur = w
		}
		for len([]rune(cur)) > width { // word longer than the line
			r := []rune(cur)
			lines = append(lines, string(r[:width]))
			cur = string(r[width:])
		}
	}
	if cur != "" {
		push()
	}
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		last := []rune(lines[maxLines-1])
		if len(last) >= width {
			last = last[:width-1]
		}
		lines[maxLines-1] = string(last) + "…"
	}
	return lines
}

// runResumeTUI is the default resumePicker: it runs the bubbletea
// program against the given terminal in/out and reports the chosen
// candidate index (ok=false on cancel). Requires a real TTY, so it's
// exercised end-to-end rather than in unit tests; the selection logic
// lives in resumeModel.Update, which is unit-tested directly.
func runResumeTUI(cands []resumeCandidate, in io.Reader, out io.Writer) (int, bool, error) {
	final, err := tea.NewProgram(
		newResumeModel(cands),
		tea.WithInput(in),
		tea.WithOutput(out),
		tea.WithAltScreen(),
	).Run()
	if err != nil {
		return 0, false, err
	}
	m, ok := final.(resumeModel)
	if !ok || m.chosen < 0 {
		return 0, false, nil
	}
	return m.chosen, true, nil
}
