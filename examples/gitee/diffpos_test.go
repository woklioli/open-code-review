// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"fmt"
	"testing"
)

func TestBuildLinePositionsSingleHunk(t *testing.T) {
	patch := "@@ -1,4 +1,5 @@\n context1\n-old line\n+new line1\n+new line2\n context2\n"
	got := buildLinePositions(patch)

	// The header itself is the origin, so the first line below it is position
	// 1. A removed line consumes a position but is not commentable, because
	// Gitee's API has no side parameter to say which side it refers to.
	want := map[int]int{1: 1, 2: 3, 3: 4, 4: 5}
	assertPositions(t, got, want)
}

func TestBuildLinePositionsSecondHunkKeepsCounting(t *testing.T) {
	patch := "@@ -1,3 +1,3 @@\n a\n-b\n+c\n@@ -10,3 +10,3 @@\n d\n-e\n+f\n"
	got := buildLinePositions(patch)

	// 1->1 and 2->3 in the first hunk; the second header takes position 4 and
	// the line below it resumes at 5, with the new-file counter jumping to 10
	// as the header says.
	want := map[int]int{1: 1, 2: 3, 10: 5, 11: 7}
	assertPositions(t, got, want)
}

func TestBuildLinePositionsIgnoresThePreamble(t *testing.T) {
	patch := "diff --git a/f.ts b/f.ts\nindex 111..222 100644\n--- a/f.ts\n+++ b/f.ts\n@@ -1,2 +1,2 @@\n a\n-b\n+c\n"
	got := buildLinePositions(patch)
	// The position counter starts at the first @@, so the "diff --git" block
	// must not shift every position by four.
	want := map[int]int{1: 1, 2: 3}
	assertPositions(t, got, want)
}

// The marker only reads as a content line if it is not handled: putting a
// second hunk after it means a miscount would shift every position in that
// hunk rather than merely adding a line nobody comments on.
func TestBuildLinePositionsSkipsNoNewlineMarker(t *testing.T) {
	patch := "@@ -1,1 +1,2 @@\n a\n+b\n\\ No newline at end of file\n@@ -10,1 +11,1 @@\n c\n"
	got := buildLinePositions(patch)
	// 1->1, 2->2, then the second header takes position 3 and its line 11
	// lands at 4. Counting the marker would make it 5.
	want := map[int]int{1: 1, 2: 2, 11: 4}
	assertPositions(t, got, want)
}

func TestBuildLinePositionsEmptyPatch(t *testing.T) {
	for name, patch := range map[string]string{"empty": "", "whitespace": "   \n  "} {
		t.Run(name, func(t *testing.T) {
			if got := buildLinePositions(patch); len(got) != 0 {
				t.Fatalf("want no positions, got %v", got)
			}
		})
	}
}

func TestBuildLinePositionsRejectsMalformedHeader(t *testing.T) {
	// A header whose new-side start is not a number cannot be walked; returning
	// nothing is safer than returning positions derived from a wrong base.
	if got := buildLinePositions("@@ -1,2 +x,3 @@\n a\n"); len(got) != 0 {
		t.Fatalf("want no positions, got %v", got)
	}
}

// A header with no explicit count is legal in unified diff and must parse.
func TestBuildLinePositionsHeaderWithoutCount(t *testing.T) {
	got := buildLinePositions("@@ -5 +5 @@\n a\n b\n")
	if p, ok := got[5]; !ok || p.Position != 1 {
		t.Fatalf("want line 5 at position 1, got %v", got)
	}
}

func TestAnchorPositionPrefersTheStartLine(t *testing.T) {
	positions := map[int]LinePosition{10: {10, 4}, 11: {11, 5}, 12: {12, 6}}
	got, ok := anchorPosition(positions, 10, 12)
	if !ok || got.Line != 10 || got.Position != 4 {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}

func TestAnchorPositionFallsBackToTheEndLine(t *testing.T) {
	// A range that begins above the hunk: the start is unreachable but the end
	// is inside it, and a comment on the overlapping part still makes sense.
	positions := map[int]LinePosition{12: {12, 6}}
	got, ok := anchorPosition(positions, 8, 12)
	if !ok || got.Line != 12 {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}

func TestAnchorPositionFallsBackToALineBetween(t *testing.T) {
	positions := map[int]LinePosition{11: {11, 5}}
	got, ok := anchorPosition(positions, 10, 12)
	if !ok || got.Line != 11 {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}

func TestAnchorPositionNormalisesReversedRanges(t *testing.T) {
	positions := map[int]LinePosition{3: {3, 2}}
	got, ok := anchorPosition(positions, 5, 3)
	if !ok || got.Line != 3 {
		t.Fatalf("a reversed range should be normalised, got %+v ok=%v", got, ok)
	}
}

func TestAnchorPositionHandlesMissingBounds(t *testing.T) {
	positions := map[int]LinePosition{7: {7, 3}}

	t.Run("no line information", func(t *testing.T) {
		if _, ok := anchorPosition(positions, 0, 0); ok {
			t.Fatal("a comment with no line at all cannot be anchored")
		}
	})
	t.Run("end only", func(t *testing.T) {
		got, ok := anchorPosition(positions, 0, 7)
		if !ok || got.Line != 7 {
			t.Fatalf("got %+v ok=%v", got, ok)
		}
	})
	t.Run("start only", func(t *testing.T) {
		got, ok := anchorPosition(positions, 7, 0)
		if !ok || got.Line != 7 {
			t.Fatalf("got %+v ok=%v", got, ok)
		}
	})
	t.Run("negative", func(t *testing.T) {
		if _, ok := anchorPosition(positions, -3, -1); ok {
			t.Fatal("negative lines cannot be anchored")
		}
	})
	t.Run("line outside the diff", func(t *testing.T) {
		if _, ok := anchorPosition(positions, 100, 200); ok {
			t.Fatal("a line the diff does not contain cannot be anchored")
		}
	})
}

// The whole point of building the map is that a position is not a line
// number. Anchoring the same finding at consecutive lines must yield distinct
// positions, or every comment would land in the same place.
func TestAnchorPositionDistinguishesAdjacentLines(t *testing.T) {
	patch := "@@ -1,3 +1,3 @@\n a\n b\n c\n"
	positions := buildLinePositions(patch)
	seen := map[int]int{}
	for line := 1; line <= 3; line++ {
		got, ok := anchorPosition(positions, line, line)
		if !ok {
			t.Fatalf("line %d should be anchorable", line)
		}
		if prev, dup := seen[got.Position]; dup {
			t.Fatalf("lines %d and %d share position %d", prev, line, got.Position)
		}
		seen[got.Position] = line
	}
}

func assertPositions(t *testing.T, got map[int]LinePosition, want map[int]int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d positions %s, want %d %v", len(got), render(got), len(want), want)
	}
	for line, position := range want {
		entry, ok := got[line]
		if !ok {
			t.Errorf("line %d missing from %s", line, render(got))
			continue
		}
		if entry.Position != position {
			t.Errorf("line %d: position %d, want %d", line, entry.Position, position)
		}
		if entry.Line != line {
			t.Errorf("line %d: entry carries line %d", line, entry.Line)
		}
	}
}

func render(m map[int]LinePosition) string {
	out := ""
	for line, entry := range m {
		out += fmt.Sprintf("%d->%d ", line, entry.Position)
	}
	return out
}
