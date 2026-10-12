// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"strings"
)

// dryRunAPI makes the whole posting path run without touching the merge
// request.
//
// Reads are delegated, so a dry run computes real positions from the real diff
// and finds the real summary comment -- which is the only way to tell whether
// the comments it would post land where they should. Writes are logged in full
// and answered with a synthetic response, so every classification and
// formatting decision downstream still executes.
type dryRunAPI struct {
	inner posterAPI
	logf  func(format string, args ...any)
}

func (d *dryRunAPI) ListPRFiles(ctx context.Context, owner, repo string, number int) ([]pullRequestFile, error) {
	return d.inner.ListPRFiles(ctx, owner, repo, number)
}

func (d *dryRunAPI) ListPRComments(ctx context.Context, owner, repo string, number int) ([]pullRequestComment, error) {
	return d.inner.ListPRComments(ctx, owner, repo, number)
}

func (d *dryRunAPI) CreateLineComment(_ context.Context, _, _ string, _ int, body, commitID, filePath string, position int) (*pullRequestComment, error) {
	d.logf("DRY RUN: would annotate %s at position %d (commit %s)\n%s",
		filePath, position, shortSHA(commitID), indent(body))
	// No new_line comes back, so the position self-check stays silent rather
	// than reporting a pass it did not observe.
	return &pullRequestComment{Path: filePath, Position: &position, CommitID: commitID}, nil
}

func (d *dryRunAPI) CreateSummaryComment(_ context.Context, _, _ string, _ int, body string) (*pullRequestComment, error) {
	d.logf("DRY RUN: would create the summary comment\n%s", indent(body))
	return &pullRequestComment{Body: body}, nil
}

func (d *dryRunAPI) UpdateComment(_ context.Context, _, _ string, commentID int64, body string) error {
	d.logf("DRY RUN: would rewrite summary comment %d\n%s", commentID, indent(body))
	return nil
}

// indent offsets a multi-line body so it reads as one block in the log.
func indent(body string) string {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	for i, line := range lines {
		lines[i] = "    " + line
	}
	return strings.Join(lines, "\n")
}
