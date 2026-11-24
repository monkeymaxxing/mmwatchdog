package store

import (
	"slices"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
)

// An update with several exact findings lists those that were not
// announced before first, each part in the order of the exact findings'
// text, and counts the exact findings that do not fit, with their nodes.
func TestAnUpdateListsNewExactFindingsFirstThenByText(t *testing.T) {
	t.Parallel()
	short := func(letter string) string { return letter + " " + strings.Repeat("x", 100) }
	long := func(letter string) string { return letter + " " + strings.Repeat("x", 5000) }
	a, b, c, d, e, f, g := short("a"), short("b"), long("c"), short("d"), long("e"), long("f"), short("g")
	announced := []string{a, c, e, f, g}
	// The pending changes arrive in no particular order. f has two nodes,
	// and a node that recovered is not listed under an exact finding.
	pending := []pendingNode{
		{node: "n-g", change: pendingJoined, severity: "warning", exactFinding: g},
		{node: "n-e", change: pendingJoined, severity: "warning", exactFinding: e},
		{node: "n-d", change: pendingJoined, severity: "warning", exactFinding: d},
		{node: "n-f1", change: pendingJoined, severity: "warning", exactFinding: f},
		{node: "n-c", change: pendingJoined, severity: "warning", exactFinding: c},
		{node: "n-ok", change: pendingRecovered, severity: "warning", exactFinding: b},
		{node: "n-b", change: pendingJoined, severity: "warning", exactFinding: b},
		{node: "n-a", change: pendingJoined, severity: "warning", exactFinding: a},
		{node: "n-f2", change: pendingChanged, severity: "warning", exactFinding: f},
	}
	about := checksandalerts.Event{CheckGroup: "packages", Check: "flaws"}
	update := findingNodesUpdate("key", about, pending, announced, QueuedBy{Node: "hub"}, time.Unix(1_800_000_000, 0))

	// b and d are new; a, c, and e were announced. f does not fit in the
	// message, and the list stops there, so g is counted with f.
	listed := []string{}
	for _, group := range update.NodesByExactFinding {
		listed = append(listed, group.ExactFinding)
	}
	if want := []string{b, d, a, c, e}; !slices.Equal(listed, want) {
		t.Fatalf("listed exact findings = %q, want %q", firstLetters(listed), firstLetters(want))
	}
	lines := strings.Split(update.Body, "\n")
	blocks := []string{}
	for _, line := range lines {
		if strings.HasPrefix(line, "new: ") || strings.HasPrefix(line, "a ") || strings.HasPrefix(line, "c ") || strings.HasPrefix(line, "e ") {
			blocks = append(blocks, line[:min(len(line), 7)])
		}
	}
	if want := []string{"new: b ", "new: d ", "a " + strings.Repeat("x", 5), "c " + strings.Repeat("x", 5), "e " + strings.Repeat("x", 5)}; !slices.Equal(blocks, want) {
		t.Fatalf("exact finding lines start %q, want %q", blocks, want)
	}
	if !slices.Contains(lines, "and 2 more exact findings on 3 nodes") {
		t.Fatalf("body has no line %q:\n%s", "and 2 more exact findings on 3 nodes", update.Body)
	}
	if len(update.Body) > UpdateMessageLimit {
		t.Fatalf("body has %d bytes, more than %d", len(update.Body), UpdateMessageLimit)
	}
}

func firstLetters(exactFindings []string) []string {
	letters := []string{}
	for _, exactFinding := range exactFindings {
		letters = append(letters, exactFinding[:1])
	}
	return letters
}
