// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The dry run exists to show what would be sent, so it has to send nothing
// while still reporting accurately.
func TestDryRunPostsNothing(t *testing.T) {
	inner := &fakeAPI{files: oneFileDiff()}
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	api := &dryRunAPI{inner: inner, logf: logf}
	p := newPoster(api, logf)

	report := &ocrReport{Comments: []ocrComment{
		{Path: "src/a.ts", Content: "in the diff", StartLine: 2, EndLine: 2},
		{Path: "src/gone.ts", Content: "not in the diff", StartLine: 1, EndLine: 1},
	}}
	result, err := p.Post(context.Background(), testJob(), "deadbeef", report, postOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(inner.created) != 0 || len(inner.summary) != 0 || len(inner.updated) != 0 {
		t.Fatalf("a dry run wrote to the API: %+v %v %+v", inner.created, inner.summary, inner.updated)
	}
	// The classification is the whole point, so it must still have happened.
	if result.Inline != 1 || len(result.Folded) != 1 {
		t.Fatalf("result = %+v", result)
	}

	joined := strings.Join(logs, "\n")
	for _, want := range []string{
		"DRY RUN: would annotate src/a.ts at position 3",
		"in the diff",
		"not in the diff",
		"DRY RUN: would create the summary comment",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in the log:\n%s", want, joined)
		}
	}
}

func TestDryRunRewritesAnExistingSummary(t *testing.T) {
	inner := &fakeAPI{
		files:    oneFileDiff(),
		comments: []pullRequestComment{{ID: 42, Body: summaryMarker}},
	}
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	p := newPoster(&dryRunAPI{inner: inner, logf: logf}, logf)

	if _, err := p.Post(context.Background(), testJob(), "head", &ocrReport{}, postOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(inner.updated) != 0 {
		t.Fatal("a dry run must not rewrite the summary")
	}
	if !strings.Contains(strings.Join(logs, "\n"), "would rewrite summary comment 42") {
		t.Errorf("the dry run should name the comment it would rewrite: %v", logs)
	}
}

// A reader of a dry run needs to see the real diff, so reads are delegated.
func TestDryRunReadsThroughToTheAPI(t *testing.T) {
	inner := &fakeAPI{files: oneFileDiff()}
	api := &dryRunAPI{inner: inner, logf: func(string, ...any) {}}

	files, err := api.ListPRFiles(context.Background(), "acme", "widget", 7)
	if err != nil || len(files) != 1 {
		t.Fatalf("got %v err=%v", files, err)
	}
	if _, err := api.ListPRComments(context.Background(), "acme", "widget", 7); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDryRunPropagatesReadFailures(t *testing.T) {
	api := &dryRunAPI{inner: &fakeAPI{filesErr: errors.New("boom")}, logf: func(string, ...any) {}}
	if _, err := api.ListPRFiles(context.Background(), "acme", "widget", 7); err == nil {
		t.Fatal("want the error to surface")
	}
}

// A dry run cannot observe where Gitee filed a comment, so it must not report
// a position as verified.
func TestDryRunLeavesThePositionUnverified(t *testing.T) {
	inner := &fakeAPI{files: oneFileDiff()}
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	p := newPoster(&dryRunAPI{inner: inner, logf: logf}, logf)

	report := &ocrReport{Comments: []ocrComment{{Path: "src/a.ts", Content: "x", StartLine: 1, EndLine: 1}}}
	if _, err := p.Post(context.Background(), testJob(), "head", report, postOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(strings.Join(logs, "\n"), "position model looks wrong") {
		t.Error("a dry run has no evidence either way and must stay silent")
	}
}

func TestIndentOffsetsEveryLine(t *testing.T) {
	got := indent("one\ntwo\n")
	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "    ") {
			t.Errorf("line %q is not indented", line)
		}
	}
}
