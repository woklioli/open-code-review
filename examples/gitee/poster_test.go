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

// fakeAPI stands in for Gitee. Every write is recorded so a test can assert
// both what was sent and what was deliberately not sent.
type fakeAPI struct {
	files    []pullRequestFile
	comments []pullRequestComment

	filesErr    error
	commentsErr error
	lineErr     error
	summaryErr  error
	updateErr   error

	created []sentLine
	summary []string
	updated []sentUpdate

	// nextLine, when set, is the new_line Gitee reports for the next line
	// comment, which drives the position self-check.
	nextLine *int
}

type sentLine struct {
	Body, CommitID, Path string
	Position             int
}

type sentUpdate struct {
	ID   int64
	Body string
}

func (f *fakeAPI) ListPRFiles(context.Context, string, string, int) ([]pullRequestFile, error) {
	return f.files, f.filesErr
}

func (f *fakeAPI) ListPRComments(context.Context, string, string, int) ([]pullRequestComment, error) {
	return f.comments, f.commentsErr
}

func (f *fakeAPI) CreateLineComment(_ context.Context, _, _ string, _ int, body, commitID, path string, position int) (*pullRequestComment, error) {
	if f.lineErr != nil {
		return nil, f.lineErr
	}
	f.created = append(f.created, sentLine{Body: body, CommitID: commitID, Path: path, Position: position})
	return &pullRequestComment{ID: int64(len(f.created)), NewLine: f.nextLine}, nil
}

func (f *fakeAPI) CreateSummaryComment(_ context.Context, _, _ string, _ int, body string) (*pullRequestComment, error) {
	if f.summaryErr != nil {
		return nil, f.summaryErr
	}
	f.summary = append(f.summary, body)
	return &pullRequestComment{ID: 500, Body: body}, nil
}

func (f *fakeAPI) UpdateComment(_ context.Context, _, _ string, id int64, body string) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updated = append(f.updated, sentUpdate{ID: id, Body: body})
	return nil
}

func testPoster(t *testing.T, api posterAPI) (*poster, *[]string) {
	t.Helper()
	var logs []string
	return newPoster(api, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}), &logs
}

func testJob() *reviewJob {
	return &reviewJob{Owner: "acme", Repo: "widget", Number: 7, Title: "Add the thing"}
}

func intPtr(n int) *int { return &n }

// oneFileDiff is a patch whose new-file lines 1..3 are commentable.
func oneFileDiff() []pullRequestFile {
	return []pullRequestFile{{
		Filename: "src/a.ts",
		Patch:    patchField{Diff: "@@ -1,3 +1,3 @@\n a\n-b\n+c\n"},
	}}
}

func TestPostAnchorsAFindingOnItsLine(t *testing.T) {
	// Line 2 is the added "c", which the patch puts at position 3, and Gitee
	// confirms it filed the comment there.
	api := &fakeAPI{files: oneFileDiff(), nextLine: intPtr(2)}
	p, _ := testPoster(t, api)

	report := &ocrReport{Comments: []ocrComment{{Path: "src/a.ts", Content: "look here", StartLine: 2, EndLine: 2}}}
	result, err := p.Post(context.Background(), testJob(), "deadbeef", report, postOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Inline != 1 || len(result.Folded) != 0 {
		t.Fatalf("result = %+v", result)
	}
	if len(api.created) != 1 {
		t.Fatalf("created %d comments", len(api.created))
	}
	got := api.created[0]
	if got.Position != 3 || got.Path != "src/a.ts" || got.CommitID != "deadbeef" {
		t.Errorf("sent %+v", got)
	}
	if !strings.Contains(got.Body, "look here") {
		t.Errorf("body lost the finding: %q", got.Body)
	}
}

func TestPostPinsCommentsToTheReviewedCommit(t *testing.T) {
	api := &fakeAPI{files: oneFileDiff()}
	p, _ := testPoster(t, api)
	report := &ocrReport{Comments: []ocrComment{{Path: "src/a.ts", Content: "x", StartLine: 1, EndLine: 1}}}
	if _, err := p.Post(context.Background(), testJob(), "resolved-head", report, postOptions{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The branch head may have moved on since; a position is only meaningful
	// against the diff it was computed from.
	if api.created[0].CommitID != "resolved-head" {
		t.Errorf("commit_id = %q", api.created[0].CommitID)
	}
}

func TestPostFoldsFindingsTheDiffCannotCarry(t *testing.T) {
	api := &fakeAPI{files: oneFileDiff()}
	p, _ := testPoster(t, api)

	report := &ocrReport{Comments: []ocrComment{
		{Path: "src/a.ts", Content: "in the diff", StartLine: 1, EndLine: 1},
		{Path: "src/a.ts", Content: "far below the hunk", StartLine: 900, EndLine: 901},
		{Path: "src/new.ts", Content: "a file the diff omits", StartLine: 1, EndLine: 1},
		{Path: "src/a.ts", Content: "no line at all"},
	}}
	result, err := p.Post(context.Background(), testJob(), "head", report, postOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Inline != 1 {
		t.Errorf("inline = %d, want 1", result.Inline)
	}
	if len(result.Folded) != 3 {
		t.Fatalf("folded = %d, want 3", len(result.Folded))
	}
	summary := api.summary[0]
	for _, want := range []string{"far below the hunk", "a file the diff omits", "no line at all"} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary must list %q:\n%s", want, summary)
		}
	}
}

func TestPostSkipsFindingsAnEarlierRunPosted(t *testing.T) {
	finding := ocrComment{Path: "src/a.ts", Content: "already said", StartLine: 1, EndLine: 1}
	api := &fakeAPI{
		files:    oneFileDiff(),
		comments: []pullRequestComment{{ID: 1, Body: renderFinding(finding)}},
	}
	p, _ := testPoster(t, api)

	result, err := p.Post(context.Background(), testJob(), "head", &ocrReport{Comments: []ocrComment{finding}}, postOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Skipped != 1 || result.Inline != 0 {
		t.Fatalf("result = %+v", result)
	}
	if len(api.created) != 0 {
		t.Fatal("a finding already on the merge request must not be posted twice")
	}
}

func TestPostRewritesTheExistingSummary(t *testing.T) {
	api := &fakeAPI{
		files: oneFileDiff(),
		comments: []pullRequestComment{
			{ID: 3, Body: "unrelated discussion"},
			{ID: 42, Body: summaryMarker + "\n## Automated review\n\nold"},
		},
	}
	p, _ := testPoster(t, api)

	result, err := p.Post(context.Background(), testJob(), "head", &ocrReport{}, postOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.SummaryID != 42 {
		t.Errorf("SummaryID = %d, want the existing comment", result.SummaryID)
	}
	if len(api.updated) != 1 || api.updated[0].ID != 42 {
		t.Fatalf("updated = %+v", api.updated)
	}
	if len(api.summary) != 0 {
		t.Fatal("an existing summary must be rewritten, not posted again")
	}
}

// A rejected line comment is a normal outcome -- the diff moved, the position
// was stale -- and it must not take the rest of the review down with it.
func TestPostKeepsGoingAfterARejectedLine(t *testing.T) {
	api := &fakeAPI{files: oneFileDiff(), lineErr: errors.New("position is invalid")}
	p, logs := testPoster(t, api)

	report := &ocrReport{Comments: []ocrComment{
		{Path: "src/a.ts", Content: "first", StartLine: 1, EndLine: 1},
		{Path: "src/a.ts", Content: "second", StartLine: 2, EndLine: 2},
	}}
	result, err := p.Post(context.Background(), testJob(), "head", report, postOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Inline != 0 || len(result.Folded) != 2 {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(strings.Join(*logs, "\n"), "position is invalid") {
		t.Errorf("the rejection should be logged, got %v", *logs)
	}
	// Everything still has to reach the reader somehow.
	summary := api.summary[0]
	if !strings.Contains(summary, "first") || !strings.Contains(summary, "second") {
		t.Errorf("both findings belong in the summary:\n%s", summary)
	}
}

// The self-check: if Gitee files the comment somewhere other than the line we
// meant, the position model is wrong and annotating the rest would be guessing.
func TestPostStopsAnnotatingWhenThePositionModelIsWrong(t *testing.T) {
	api := &fakeAPI{files: oneFileDiff(), nextLine: intPtr(99)}
	p, logs := testPoster(t, api)

	report := &ocrReport{Comments: []ocrComment{
		{Path: "src/a.ts", Content: "first", StartLine: 1, EndLine: 1},
		{Path: "src/a.ts", Content: "second", StartLine: 2, EndLine: 2},
	}}
	result, err := p.Post(context.Background(), testJob(), "head", report, postOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Inline != 1 {
		t.Errorf("the first comment is already sent, so inline = 1, got %d", result.Inline)
	}
	if len(result.Folded) != 1 {
		t.Fatalf("the rest must be listed instead of annotated, got %+v", result.Folded)
	}
	joined := strings.Join(*logs, "\n")
	if !strings.Contains(joined, "position model looks wrong") {
		t.Errorf("the mismatch must be reported loudly, got: %v", *logs)
	}
	if !strings.Contains(api.summary[0], "second") {
		t.Error("the unanchored finding must still be listed in the summary")
	}
}

// A response with no new_line cannot confirm or refute the model, so it must
// not be read as a mismatch.
func TestPostToleratesAMissingNewLine(t *testing.T) {
	api := &fakeAPI{files: oneFileDiff()}
	p, logs := testPoster(t, api)

	report := &ocrReport{Comments: []ocrComment{
		{Path: "src/a.ts", Content: "first", StartLine: 1, EndLine: 1},
		{Path: "src/a.ts", Content: "second", StartLine: 2, EndLine: 2},
	}}
	result, err := p.Post(context.Background(), testJob(), "head", report, postOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Inline != 2 || len(result.Folded) != 0 {
		t.Fatalf("result = %+v", result)
	}
	if len(*logs) != 0 {
		t.Errorf("nothing should be reported, got %v", *logs)
	}
}

// The check runs once per poster, so a second review must not re-litigate it.
func TestPositionSelfCheckRunsOnce(t *testing.T) {
	api := &fakeAPI{files: oneFileDiff(), nextLine: intPtr(99)}
	p, logs := testPoster(t, api)

	for range 2 {
		report := &ocrReport{Comments: []ocrComment{{Path: "src/a.ts", Content: "x", StartLine: 1, EndLine: 1}}}
		if _, err := p.Post(context.Background(), testJob(), "head", report, postOptions{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if got := len(*logs); got != 1 {
		t.Errorf("warned %d times, want 1: %v", got, *logs)
	}
}

func TestPostReportsReadFailures(t *testing.T) {
	cases := map[string]*fakeAPI{
		"files":    {filesErr: errors.New("boom")},
		"comments": {files: oneFileDiff(), commentsErr: errors.New("boom")},
		"summary":  {files: oneFileDiff(), summaryErr: errors.New("boom")},
	}
	for name, api := range cases {
		t.Run(name, func(t *testing.T) {
			p, _ := testPoster(t, api)
			if _, err := p.Post(context.Background(), testJob(), "head", &ocrReport{}, postOptions{}); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestPostReportsAFailedSummaryRewrite(t *testing.T) {
	api := &fakeAPI{
		files:     oneFileDiff(),
		comments:  []pullRequestComment{{ID: 42, Body: summaryMarker}},
		updateErr: errors.New("boom"),
	}
	p, _ := testPoster(t, api)
	if _, err := p.Post(context.Background(), testJob(), "head", &ocrReport{}, postOptions{}); err == nil {
		t.Fatal("want an error")
	}
}

func TestRenderFindingIncludesTheSuggestion(t *testing.T) {
	body := renderFinding(ocrComment{
		Path: "src/a.go", Content: "this leaks", StartLine: 4, EndLine: 4,
		Category: "bug", Severity: "high", SuggestionCode: "defer f.Close()",
	})
	for _, want := range []string{"**bug**", "severity: high", "this leaks", "```go", "defer f.Close()", inlinePrefix} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	// The marker has to lead, so a re-readable body identifies itself first.
	if !strings.HasPrefix(body, inlinePrefix) {
		t.Errorf("the marker must come first:\n%s", body)
	}
}

func TestRenderFindingOmitsWhatOcrDidNotSay(t *testing.T) {
	body := renderFinding(ocrComment{Path: "a.ts", Content: "just this"})
	if strings.Contains(body, "Suggested change") {
		t.Errorf("an empty suggestion must not produce a code block:\n%s", body)
	}
	if strings.Contains(body, "severity") || strings.Contains(body, "**") {
		t.Errorf("an unlabelled finding must not invent a header:\n%s", body)
	}
}

// The marker keys on the finding, not its place in the review, so reordering
// the report must not make an already-posted comment look new.
func TestFindingMarkerIsStableAcrossReordering(t *testing.T) {
	comment := ocrComment{Path: "a.ts", Content: "x", StartLine: 1, EndLine: 2}
	if findingMarker(comment) != findingMarker(comment) {
		t.Fatal("the marker must be deterministic")
	}
	other := ocrComment{Path: "a.ts", Content: "x", StartLine: 1, EndLine: 3}
	if findingMarker(comment) == findingMarker(other) {
		t.Fatal("a different range is a different finding")
	}
	// A trailing newline in the content is not a different finding, but a
	// changed one is.
	changed := comment
	changed.Content = "y"
	if findingMarker(comment) == findingMarker(changed) {
		t.Fatal("different content is a different finding")
	}
}

func TestMarkersInFindsEveryMarker(t *testing.T) {
	body := inlinePrefix + "aaa -->\ntext\n" + inlinePrefix + "bbb -->\nmore"
	got := markersIn(body)
	if len(got) != 2 || got[0] != inlinePrefix+"aaa -->" || got[1] != inlinePrefix+"bbb -->" {
		t.Fatalf("got %v", got)
	}
	for name, input := range map[string]string{
		"none":      "just prose",
		"truncated": inlinePrefix + "unterminated",
	} {
		t.Run(name, func(t *testing.T) {
			if got := markersIn(input); len(got) != 0 {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestRenderSummaryReportsCountsAndWarnings(t *testing.T) {
	body := renderSummary(summaryInput{
		Job:      testJob(),
		CommitID: "0123456789abcdef",
		Report: &ocrReport{
			Summary:        &ocrSummary{FilesReviewed: 39, Comments: 2, Elapsed: "8m18s"},
			ProjectSummary: "Overall this is fine.",
			Warnings:       []ocrWarning{{File: "src/b.ts", Type: "skipped", Message: "too large"}},
		},
		Result:  &postResult{Inline: 3, Folded: []ocrComment{{Path: "x.ts", Content: "folded one"}}, Skipped: 1},
		Options: postOptions{Notes: []string{"could not confirm the `retry` prop"}, Dropped: 2},
	})
	for _, want := range []string{
		summaryMarker, "01234567", "acme/widget#7", "Annotated in the diff: 3", "Listed below: 1",
		"Already reported: 1", "Removed as contradicted by a pinned dependency: 2",
		"Reviewed: 39 files in 8m18s", "Overall this is fine.", "folded one", "too large",
		"could not confirm the `retry` prop",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	// The full sha is noise in a comment; the short form identifies the
	// revision well enough.
	if strings.Contains(body, "0123456789abcdef") {
		t.Error("the summary should use the short sha")
	}
}

func TestRenderSummaryOmitsAbsentSections(t *testing.T) {
	body := renderSummary(summaryInput{Job: testJob(), CommitID: "abc", Report: &ocrReport{}, Result: &postResult{}})
	for _, unwanted := range []string{"Listed below", "Already reported", "### Findings", "### Warnings", "### Unconfirmed"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("an empty review should not render %q:\n%s", unwanted, body)
		}
	}
	if !strings.Contains(body, "Annotated in the diff: 0") {
		t.Errorf("the zero count is the point of the summary:\n%s", body)
	}
}

func TestRenderSummarySurvivesAMissingReport(t *testing.T) {
	// A run that failed before it produced a document still has to leave a
	// trace, rather than panicking on the way out.
	body := renderSummary(summaryInput{Job: testJob(), CommitID: "abc", Result: &postResult{}})
	if !strings.Contains(body, summaryMarker) {
		t.Fatalf("got:\n%s", body)
	}
}

func TestCommentLocation(t *testing.T) {
	cases := map[string]struct {
		comment ocrComment
		want    string
	}{
		"no line":     {ocrComment{Path: "a.ts"}, "a.ts"},
		"single":      {ocrComment{Path: "a.ts", StartLine: 4}, "a.ts:4"},
		"range":       {ocrComment{Path: "a.ts", StartLine: 4, EndLine: 9}, "a.ts:4-9"},
		"open range":  {ocrComment{Path: "a.ts", StartLine: 4, EndLine: 4}, "a.ts:4"},
		"end only":    {ocrComment{Path: "a.ts", EndLine: 4}, "a.ts"},
		"default arg": {ocrComment{Path: "a.ts", StartLine: 1, EndLine: 2}, "a.ts:1-2"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := commentLocation(tc.comment); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFenceLanguage(t *testing.T) {
	cases := map[string]string{
		"a/b/c.go": "go", "X.TS": "typescript", "a.tsx": "tsx",
		"style.css": "css", "unknown.zzz": "", "Makefile": "",
	}
	for file, want := range cases {
		if got := fenceLanguage(file); got != want {
			t.Errorf("fenceLanguage(%q) = %q, want %q", file, got, want)
		}
	}
}

func TestShortSHA(t *testing.T) {
	if got := shortSHA("0123456789abcdef"); got != "01234567" {
		t.Errorf("got %q", got)
	}
	if got := shortSHA("abc"); got != "abc" {
		t.Errorf("a short sha should pass through, got %q", got)
	}
}
