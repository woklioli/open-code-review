// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"regexp"
	"strconv"
	"strings"
)

// Gitee's comment API accepts a single `position`, documented only as the
// "line number in the PR's code review diff". That wording is GitHub's legacy
// position, and Gitee's comment model is a field-for-field copy of GitHub v3
// (path, position, original_position, new_line, commit_id, original_commit_id),
// so this reads it the GitHub way: the number of lines below the first `@@`
// hunk header.
//
// This could not be confirmed against live data. Reading comments back never
// fills in `position` -- a sweep of 571 merge requests across 33 repositories
// found none, and Gitee stores the absolute line in `new_line` instead -- so
// there is no observed (position, new_line) pair to check the model against.
// The mitigation is at the call site: after the first successful post the
// service compares the returned new_line with the line it meant to comment on,
// and stops annotating when they disagree. See poster.Post.
var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// LinePosition is where a comment landed: the new-file line it is anchored to
// and the diff position that names it.
type LinePosition struct {
	Line     int
	Position int
}

// buildLinePositions maps each commentable line of one file's patch to its
// diff position.
//
// Only lines that exist on the new side are recorded: context and added lines.
// A removed line lives only on the old side, and Gitee's comment API has no
// side parameter to say so, which makes a position on one unaddressable.
func buildLinePositions(patch string) map[int]LinePosition {
	positions := map[int]LinePosition{}
	if strings.TrimSpace(patch) == "" {
		return positions
	}

	// Split on the trailing newline's account: a patch text ends with one, and
	// the empty element it produces would otherwise read as a context line and
	// map one line past the end of the file. A genuinely blank context line
	// arrives as a single space, not as an empty string.
	lines := strings.Split(strings.TrimSuffix(patch, "\n"), "\n")
	firstHunk := -1
	position := 0
	newLine := 0
	inHunk := false

	for index, line := range lines {
		if match := hunkHeader.FindStringSubmatch(line); match != nil {
			if firstHunk < 0 {
				// The first header is the origin the positions count from, so
				// it does not consume one itself.
				firstHunk = index
				position = 0
			} else {
				position++
			}
			// The regex captures only digits, so the conversion cannot fail.
			start, _ := strconv.Atoi(match[1])
			newLine = start
			inHunk = true
			continue
		}
		if !inHunk {
			// Anything before the first header -- the "diff --git" and
			// "+++ b/..." preamble -- is not part of the positioned region.
			continue
		}

		position++

		// The marker git emits when the last line of a file has no trailing
		// newline. It belongs to the line above it and advances nothing.
		if strings.HasPrefix(line, `\`) {
			position--
			continue
		}

		switch {
		case strings.HasPrefix(line, "+"):
			positions[newLine] = LinePosition{Line: newLine, Position: position}
			newLine++
		case strings.HasPrefix(line, "-"):
			// Old side only; the new-file counter does not move.
		default:
			// Context, including the empty line that a bare "" represents.
			positions[newLine] = LinePosition{Line: newLine, Position: position}
			newLine++
		}
	}
	return positions
}

// anchorPosition picks the diff position for a comment that spans
// [startLine, endLine] in the new file.
//
// The start line is preferred: a comment describes the code the reader should
// look at first, and that is where the finding begins. The end line and then
// any line in between are fallbacks, because ocr reports ranges that can begin
// or end outside the hunk while still overlapping it.
func anchorPosition(positions map[int]LinePosition, startLine, endLine int) (LinePosition, bool) {
	if startLine > endLine {
		startLine, endLine = endLine, startLine
	}
	if startLine <= 0 && endLine <= 0 {
		return LinePosition{}, false
	}
	if startLine <= 0 {
		if p, ok := positions[endLine]; ok {
			return p, true
		}
		return LinePosition{}, false
	}
	if endLine <= 0 {
		endLine = startLine
	}

	if p, ok := positions[startLine]; ok {
		return p, true
	}
	if p, ok := positions[endLine]; ok {
		return p, true
	}
	for line := startLine + 1; line < endLine; line++ {
		if p, ok := positions[line]; ok {
			return p, true
		}
	}
	return LinePosition{}, false
}
