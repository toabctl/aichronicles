package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
	resumeSelectedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true)
	resumeDimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	resumeTitleStyle    = lipgloss.NewStyle().Bold(true)
	resumeFooterStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("244")).MarginTop(1)
)

// resumeModel is the bubbletea model behind the interactive picker: a
// session list on the left, a preview of the selected session on the
// right. chosen is the picked index after the user hits enter, or -1
// when they cancelled (q / esc / ctrl-c).
type resumeModel struct {
	cands    []resumeCandidate
	cursor   int
	chosen   int
	width    int
	height   int
	quitting bool
}

func newResumeModel(cands []resumeCandidate) resumeModel {
	// Default size lets View() render sensibly before the first
	// WindowSizeMsg arrives (and in tests, which send no size).
	return resumeModel{cands: cands, chosen: -1, width: 96, height: 24}
}

func (m resumeModel) Init() tea.Cmd { return nil }

func (m resumeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.quitting = true
			m.chosen = -1
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.cands)-1 {
				m.cursor++
			}
		case "home", "g":
			m.cursor = 0
		case "end", "G":
			m.cursor = len(m.cands) - 1
		case "enter":
			m.chosen = m.cursor
			m.quitting = true
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m resumeModel) View() string {
	if m.quitting || len(m.cands) == 0 {
		return ""
	}
	leftW, rightW := resumePaneWidths(m.width)
	body := lipgloss.JoinHorizontal(
		lipgloss.Top,
		resumeBoxStyle.Width(leftW).Render(renderResumeList(m.cands, m.cursor, leftW)),
		resumeBoxStyle.Width(rightW).Render(renderResumePreviewPane(m.cands[m.cursor], rightW)),
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

// renderResumeList renders the left column: one entry per candidate
// (index · short id · when, then a dim cwd line), with the cursor row
// highlighted. Text is truncated to the content width before styling so
// ANSI codes are never counted into — or sliced by — the rune cap.
func renderResumeList(cands []resumeCandidate, cursor, width int) string {
	now := time.Now()
	var b strings.Builder
	for i, c := range cands {
		d := c.digest
		marker := "  "
		title := truncateRunes(fmt.Sprintf("%d  %s  %s",
			i+1, preview.ShortID(d.ID), resumeWhen(d, now)), width-2)
		if i == cursor {
			marker = "▌ "
			title = resumeSelectedStyle.Render(title)
		}
		cwd := truncateRunes(flattenLine(strPtrOrDash(d.Cwd)), width-2)
		b.WriteString(marker + title + "\n")
		b.WriteString("  " + resumeDimStyle.Render(cwd) + "\n")
		if i < len(cands)-1 {
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// renderResumePreviewPane renders the right column for the selected
// candidate: cwd + when header, the opening prompt, then the trailing
// conversation (one truncated line per message, speaker-labelled).
func renderResumePreviewPane(c resumeCandidate, width int) string {
	d := c.digest
	var b strings.Builder
	b.WriteString(resumeTitleStyle.Render(truncateRunes(strPtrOrDash(d.Cwd), width)) + "\n")
	b.WriteString(resumeDimStyle.Render(resumeWhen(d, time.Now())) + "\n\n")

	if fp := strPtrOrDash(d.FirstPrompt); fp != "-" {
		b.WriteString(truncateRunes("▸ "+flattenLine(fp), width) + "\n\n")
	}

	if len(c.tail) == 0 {
		b.WriteString(resumeDimStyle.Render("(no message preview)"))
		return b.String()
	}
	for i, ev := range c.tail {
		line := fmt.Sprintf("%-5s %s", resumeRoleLabel(ev.Kind),
			flattenLine(ptrStrOrEmpty(ev.ContentText)))
		b.WriteString(truncateRunes(line, width))
		if i < len(c.tail)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
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
