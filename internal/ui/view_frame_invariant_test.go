// Frame-geometry invariants for Home.View.
//
// Bubble Tea's standard renderer positions rows with relative cursor movement
// and per-line diffing. A single physical line wrap — one line wider than the
// terminal — permanently shifts its row model against the real screen until a
// full repaint, which surfaces as ghost/duplicated sidebar rows on the next
// full-list repaint (the "scrolling corrupts the sessions list" report).
//
// The contract, enforced at the single choke point in View():
//   - the frame has EXACTLY h.height lines;
//   - every line is EXACTLY h.width cells (equality, not <= — the padding is
//     what overwrites the previous frame's glyphs on incremental repaint).
//
// These tests pin that contract for the main list frame under hostile content
// (over-wide explicit titles, CJK/emoji/keycap glyphs, nil remote rows) and —
// critically — for renderFrame's early-return views (splashes, overlays,
// dialogs, pagers), which hand-roll their own geometry and historically
// bypassed the clamp entirely.
package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/asheshgoplani/agent-deck/internal/session"
)

// assertFrameGeometry asserts the exact-height / exact-width terminal contract.
func assertFrameGeometry(t *testing.T, frame string, width, height int) {
	t.Helper()
	lines := strings.Split(frame, "\n")
	if len(lines) != height {
		t.Fatalf("frame has %d lines, want exactly %d", len(lines), height)
	}
	for i, line := range lines {
		if w := cellWidth(line); w != width {
			t.Errorf("line %d is %d cells, want exactly %d: %q", i, w, width, line)
		}
	}
}

// hostileTestHome builds a Home whose list content tries hard to overflow the
// pane: an over-long explicit (non-autoName) title mixing CJK, emoji, and
// keycap glyphs, an over-long group name, a scrolled viewport, and a remote
// item with a nil RemoteSession.
func hostileTestHome(t *testing.T, width, height int) *Home {
	t.Helper()
	longTitle := strings.Repeat("日本語タイトル🚀1️⃣", 8) + strings.Repeat("-explicit-title", 6)
	instances := []*session.Instance{
		{ID: "s1", Title: longTitle, Tool: "claude", Status: session.StatusRunning},
		{ID: "s2", Title: "short", Tool: "shell", Status: session.StatusStopped},
	}
	group := &session.Group{Name: strings.Repeat("very-long-group-name-", 8), Path: "g", Expanded: true, Sessions: instances}
	items := []session.Item{
		{Type: session.ItemTypeGroup, Group: group, Path: "g", Level: 0, RootGroupNum: 1},
		{Type: session.ItemTypeSession, Session: instances[0], Path: "g", Level: 1},
		{Type: session.ItemTypeSession, Session: instances[1], Path: "g", Level: 1},
		{Type: session.ItemTypeRemoteSession, Level: 1}, // nil RemoteSession row
		{Type: session.ItemTypeDivider, DividerLabel: "idle / done"},
		{Type: session.ItemTypeSession, Session: instances[1], Path: "g", Level: 1, IsLastInGroup: true},
	}
	h := newTestHomeWithItems(width, height, items)
	h.instances = instances
	h.instanceByID = map[string]*session.Instance{"s1": instances[0], "s2": instances[1]}
	h.groupTree = session.NewGroupTree(instances)
	h.viewOffset = 1
	return h
}

func TestViewFrameGeometry_HostileList(t *testing.T) {
	for _, size := range []struct{ w, h int }{{100, 30}, {60, 20}} {
		h := hostileTestHome(t, size.w, size.h)
		t.Run(fmt.Sprintf("%dx%d", size.w, size.h), func(t *testing.T) {
			assertFrameGeometry(t, h.View(), size.w, size.h)
		})
	}
}

// TestViewFrameGeometry_OverlayFrames covers renderFrame's early-return views:
// each hand-rolls its own layout, and before the View-level clamp any over-wide
// line there wrapped and desynced the renderer for every later frame.
func TestViewFrameGeometry_OverlayFrames(t *testing.T) {
	const width, height = 100, 30
	overWide := strings.Repeat("over-wide 日本語 content 1️⃣ ", 20)

	cases := []struct {
		name  string
		setup func(h *Home)
	}{
		{"loading splash", func(h *Home) { h.initialLoading = true }},
		{"quitting splash", func(h *Home) { h.isQuitting = true }},
		{"terminal too small", func(h *Home) { h.width, h.height = minTerminalWidth-1, minTerminalHeight-1 }},
		{"help overlay", func(h *Home) { h.helpOverlay.SetSize(h.width, h.height); h.helpOverlay.Show() }},
		{"search", func(h *Home) { h.search.SetSize(h.width, h.height); h.search.SetItems(h.instances); h.search.Show() }},
		{"global search", func(h *Home) { h.globalSearch.SetSize(h.width, h.height); h.globalSearch.Show() }},
		{"confirm dialog", func(h *Home) { h.confirmDialog.ShowNotice("Notice", overWide) }},
		{"session switcher", func(h *Home) {
			h.sessionSwitcher.SetSize(h.width, h.height)
			h.sessionSwitcher.Show("s1", h.instances, nil)
		}},
		{"scrollback pager over-wide content", func(h *Home) {
			h.scrollbackPager.Show("title", "s1", h.width, h.height)
			h.scrollbackPager.SetContent(overWide + "\n" + strings.Repeat("日", 300))
		}},
		{"cost dashboard", func(h *Home) { h.showCostDashboard = true }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hostileTestHome(t, width, height)
			tc.setup(h)
			assertFrameGeometry(t, h.View(), h.width, h.height)
		})
	}
}

// TestRenderItem_ExactlyOneLinePerItem pins the 1-item = 1-line invariant that
// renderSessionList's visibleCount accounting and mouseYToItemIndex both
// assume. The nil-RemoteSession row is the historical violator.
func TestRenderItem_ExactlyOneLinePerItem(t *testing.T) {
	h := hostileTestHome(t, 100, 30)
	// Include every renderable ItemType, plus the nil-RemoteSession row.
	items := append([]session.Item{}, h.flatItems...)
	items = append(items,
		session.Item{Type: session.ItemTypeSession, CreatingID: "c1", CreatingTitle: "creating", CreatingTool: "claude", Level: 1},
		session.Item{Type: session.ItemTypeWindow, IsWindow: true, WindowName: "win", WindowIndex: 1, WindowSessionID: "s1", Level: 2},
	)

	snapshot := h.getSessionRenderSnapshot()
	stats := h.buildGroupRenderStats(snapshot)
	for i, item := range items {
		var b strings.Builder
		h.renderItem(&b, item, false, i, stats, snapshot, 40)
		out := b.String()
		if got := strings.Count(out, "\n"); got != 1 || !strings.HasSuffix(out, "\n") {
			t.Errorf("item %d (type %v): rendered %d newline(s), want exactly one trailing newline: %q", i, item.Type, got, out)
		}
	}
}
