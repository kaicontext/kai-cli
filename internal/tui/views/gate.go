package views

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"kai/api/graph"
	"kai/api/projects"
	"kai/api/safetygate"
	"kai/api/util"
	"kai/api/workspace"
)

// heldItem pairs a held snapshot with the project it lives in so the
// gate can list EVERY project's held integrations workspace-wide (not
// just the primary's) and approve/reject each against its OWN project
// DB. project is "" in single-root workspaces (no tag rendered).
type heldItem struct {
	node    *graph.Node
	project string
	mgr     *workspace.Manager
}

// Gate is the held-integration pane. It lists snapshots whose gate
// verdict was Review or Block, lets the user navigate with arrow
// keys, approve with `a` (advance team-visible refs), reject with
// `r` (mark dismissed), and inspect with enter.
//
// All engine work goes through the same workspace.Manager methods
// `kai gate approve` / `reject` use, so behavior is identical across
// surfaces.
type Gate struct {
	db      *graph.DB
	mgr     *workspace.Manager
	// set, when non-nil, allows Refresh to scope to the
	// primary project's DB via set.Primary(). Approve/reject
	// always target the primary DB. If set or its primary is
	// nil, Refresh falls back to the single g.db path.
	set *projects.Set
	width   int
	height  int
	focused bool

	items    []heldItem
	selected int

	status string // last-action message ("approved 9f3a — snap.latest moved", etc.)
}

// NewGate builds a fresh gate view. The caller (root model) provides
// the live DB; the workspace.Manager is constructed here so the view
// is self-contained. A nil DB is tolerated (tests, no-data states):
// the pane renders an empty list until SetDB is wired to a live one.
func NewGate(db *graph.DB) Gate {
	g := Gate{db: db}
	if db != nil {
		g.mgr = workspace.NewManager(db)
		g.refresh()
	}
	return g
}

// SetProjects wires the multi-root project set so Refresh can
	// scope to the primary project's DB. Called once during TUI
	// construction, after Open() has populated the project DB.
	// Single-root callers can leave it unset; refresh then falls
	// back to the single primary DB.
func (g *Gate) SetProjects(set *projects.Set) { g.set = set }

// HeldCount reports the number of currently held items. Exposed so the
// parent model can drive the launch-time banner and mid-session
// notifications (the gate pane itself owns the source of truth, so
// callers ask it rather than re-querying the DB).
func (g *Gate) HeldCount() int { return len(g.items) }

// SetSize and Focus/Blur are the standard sub-view contract. Focus
// changes only affect input handling — rendering still happens.
func (g *Gate) SetSize(width, height int) { g.width, g.height = width, height }
func (g *Gate) Focus()                    { g.focused = true }
func (g *Gate) Blur()                     { g.focused = false }
func (g *Gate) Focused() bool             { return g.focused }

// GateRefreshedMsg is delivered to the parent loop when refresh
// completes — used to push the latest list back into Update.
type GateRefreshedMsg struct {
	items []heldItem
	err   error
}

// gateActionMsg reports the result of an approve/reject action so the
// status line can render and the list can refresh.
type gateActionMsg struct {
	kind     string // "approve" or "reject"
	snapHex  string
	advanced []string
	err      error
}

// Refresh forces a re-read of the held list. Returns a tea.Cmd so the
// parent app can trigger it (e.g. after a successful integrate from
// the REPL changes the held set). Returns nil when DB is unset so
// the pane stays inert in DB-less contexts (tests, future detached
// modes).
func (g *Gate) Refresh() tea.Cmd {
	if g.db == nil && g.set == nil {
		return nil
	}
	gdb := g.db
	set := g.set
	return func() tea.Msg {
		// Workspace-wide: list held snapshots from EVERY project, each
		// tagged with its project + a manager bound to that project's DB
		// so approve/reject target the right store. The old code scoped
		// to the PRIMARY project only, which silently HID held work in
		// sibling projects — `/gate` launched in kai/ showed "nothing
		// held" while kai-tui had 4 (the 2026-06-11 report). The per-row
		// project tag (renderRow) addresses the "which project is this?"
		// concern that originally motivated the primary-only scope.
		if set != nil {
			projs := set.Projects()
			multiRoot := len(projs) > 1
			var items []heldItem
			var firstErr error
			for _, p := range projs {
				if p.DB == nil {
					continue
				}
				held, err := safetygate.ListHeld(p.DB)
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				mgr := workspace.NewManager(p.DB)
				name := ""
				if multiRoot {
					name = p.Name
				}
				for _, n := range held {
					items = append(items, heldItem{node: n, project: name, mgr: mgr})
				}
			}
			return GateRefreshedMsg{items: items, err: firstErr}
		}
		// Single-root fallback. gdb can be nil in DB-less contexts
		// (tests, detached modes) — guard so ListHeld isn't handed a
		// nil store.
		if gdb == nil {
			return GateRefreshedMsg{}
		}
		held, err := safetygate.ListHeld(gdb)
		mgr := workspace.NewManager(gdb)
		var items []heldItem
		for _, n := range held {
			items = append(items, heldItem{node: n, mgr: mgr})
		}
		return GateRefreshedMsg{items: items, err: err}
	}
}

// refresh is the synchronous version used during construction. The
// async Refresh() above is preferred at runtime so DB reads don't
// block the Bubble Tea event loop.
func (g *Gate) refresh() {
	held, _ := safetygate.ListHeld(g.db)
	mgr := g.mgr
	if mgr == nil && g.db != nil {
		mgr = workspace.NewManager(g.db)
	}
	g.items = g.items[:0]
	for _, n := range held {
		g.items = append(g.items, heldItem{node: n, mgr: mgr})
	}
	if g.selected >= len(g.items) {
		g.selected = len(g.items) - 1
	}
	if g.selected < 0 {
		g.selected = 0
	}
}

// Update handles key input when focused, plus the async result
// messages from Refresh and from approve/reject actions.
func (g Gate) Update(msg tea.Msg) (Gate, tea.Cmd) {
	switch msg := msg.(type) {
	case GateRefreshedMsg:
		if msg.err != nil {
			g.status = "refresh error: " + msg.err.Error()
			return g, nil
		}
		g.items = msg.items
		if g.selected >= len(g.items) {
			g.selected = len(g.items) - 1
		}
		if g.selected < 0 {
			g.selected = 0
		}
		return g, nil

	case gateActionMsg:
		short := msg.snapHex
		if len(short) > 12 {
			short = short[:12]
		}
		switch {
		case msg.err != nil:
			g.status = fmt.Sprintf("%s %s failed: %v", msg.kind, short, msg.err)
		case msg.kind == "approve":
			g.status = fmt.Sprintf("approved %s — advanced %s", short, strings.Join(msg.advanced, ", "))
		case msg.kind == "reject":
			g.status = fmt.Sprintf("dismissed %s", short)
		}
		return g, g.Refresh()

	case tea.KeyMsg:
		if !g.focused {
			return g, nil
		}
		switch msg.String() {
		case "up", "k":
			if g.selected > 0 {
				g.selected--
			}
		case "down", "j":
			if g.selected < len(g.items)-1 {
				g.selected++
			}
		case "a":
			if cmd := g.approveSelected(); cmd != nil {
				return g, cmd
			}
		case "r":
			if cmd := g.rejectSelected(); cmd != nil {
				return g, cmd
			}
		}
	}
	return g, nil
}

// approveSelected dispatches an approve action on whichever snapshot
// is currently highlighted. Returns nil if nothing is selected so the
// caller can ignore the keypress.
func (g *Gate) approveSelected() tea.Cmd {
	if len(g.items) == 0 {
		return nil
	}
	it := g.items[g.selected]
	mgr := it.mgr
	if mgr == nil {
		mgr = g.mgr
	}
	if mgr == nil {
		return nil
	}
	id := it.node.ID
	return func() tea.Msg {
		advanced, err := mgr.ApproveHeld(id)
		return gateActionMsg{
			kind:     "approve",
			snapHex:  util.BytesToHex(id),
			advanced: advanced,
			err:      err,
		}
	}
}

func (g *Gate) rejectSelected() tea.Cmd {
	if len(g.items) == 0 {
		return nil
	}
	it := g.items[g.selected]
	mgr := it.mgr
	if mgr == nil {
		mgr = g.mgr
	}
	if mgr == nil {
		return nil
	}
	id := it.node.ID
	return func() tea.Msg {
		err := mgr.RejectHeld(id)
		return gateActionMsg{
			kind:    "reject",
			snapHex: util.BytesToHex(id),
			err:     err,
		}
	}
}

// View renders the held list with a one-line header and a status line
// underneath. The selected row is highlighted; the focused state
// changes the border color so the user knows which pane has input.
func (g Gate) View() string {
	header := styleHeader.Render("Gate — held integrations")
	if !g.focused {
		header = styleHeaderDim.Render("Gate — held integrations")
	}

	var body string
	if len(g.items) == 0 {
		body = styleDim.Render("  (no integrations are held)")
	} else {
		var lines []string
		for i, it := range g.items {
			lines = append(lines, g.renderRow(it, i == g.selected))
		}
		body = strings.Join(lines, "\n")
	}

	footer := ""
	if g.status != "" {
		footer = styleDim.Render(g.status)
	} else if g.focused {
		footer = styleDim.Render("a=approve  r=reject  ↑↓=move  enter=details")
	}

	frame := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Padding(0, 1).
		Width(maxInt(g.width-2, 0))
	if g.focused {
		frame = frame.BorderForeground(lipgloss.Color("12"))
	} else {
		frame = frame.BorderForeground(lipgloss.Color("8"))
	}

	inner := lipgloss.JoinVertical(lipgloss.Left, header, body, footer)
	return frame.Render(inner)
}

func (g *Gate) renderRow(it heldItem, selected bool) string {
	n := it.node
	v, _ := n.Payload["gateVerdict"].(string)
	blast, _ := n.Payload["gateBlastRadius"].(float64)
	createdMs, _ := n.Payload["createdAt"].(float64)
	when := ""
	if createdMs > 0 {
		when = humanAge(time.UnixMilli(int64(createdMs)))
	}
	id := util.BytesToHex(n.ID)
	if len(id) > 12 {
		id = id[:12]
	}

	verdict := strings.ToUpper(v)
	switch v {
	case string(safetygate.Block):
		verdict = styleError.Render(verdict)
	case string(safetygate.Review):
		verdict = styleWarn.Render(verdict)
	}

	tag := ""
	if it.project != "" {
		tag = "[" + it.project + "] "
	}
	row := fmt.Sprintf("  %s%s  %-6s  blast=%-4d  %s", tag, id, verdict, int(blast), when)
	if selected {
		return styleSelected.Render(row)
	}
	return row
}

// humanAge returns a short relative-time string ("2m", "3h", "1d").
// Intentionally compact to keep the gate row narrow.
func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// shared styles — kept in this file rather than a separate styles.go
// because there's only the one consumer right now. Promote later if
// the sync pane wants the same palette.
var (
	styleHeader    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	styleHeaderDim = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("8"))
	styleSelected  = lipgloss.NewStyle().Background(lipgloss.Color("237"))
	styleWarn      = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)
