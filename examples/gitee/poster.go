// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
)

// posterAPI is the part of the Gitee client the poster uses. It is an interface
// so a dry run can substitute a client that logs writes instead of sending
// them, and so the classification below can be tested without a server.
type posterAPI interface {
	ListPRFiles(ctx context.Context, owner, repo string, number int) ([]pullRequestFile, error)
	ListPRComments(ctx context.Context, owner, repo string, number int) ([]pullRequestComment, error)
	CreateLineComment(ctx context.Context, owner, repo string, number int, body, commitID, path string, position int) (*pullRequestComment, error)
	CreateSummaryComment(ctx context.Context, owner, repo string, number int, body string) (*pullRequestComment, error)
	UpdateComment(ctx context.Context, owner, repo string, commentID int64, body string) error
}

// Markers let a later run recognise what an earlier one wrote, which is what
// makes re-delivered webhooks and service restarts idempotent.
const (
	summaryMarker = "<!-- ocr-review-summary -->"
	inlinePrefix  = "<!-- ocr-review:"
)

// postResult counts what the run did, for logging and for the summary.
type postResult struct {
	Inline int
	// Folded findings had no anchor in the diff, so they are listed in the
	// summary instead of annotated in place.
	Folded []ocrComment
	// Skipped findings were already posted by an earlier run of the same
	// revision.
	Skipped int
	// SummaryID is the sticky summary this run created or updated.
	SummaryID int64
}

// postOptions is what the run learned beyond the review itself.
type postOptions struct {
	// Notes are findings the verification round could not settle. They are
	// reported so a reader knows a check was attempted and did not conclude,
	// which silence would misrepresent as a pass.
	Notes []string
	// Dropped counts findings the verification round removed. The count is
	// reported because a review that quietly loses findings is worse than one
	// that says how many it removed.
	Dropped int
}

type poster struct {
	api  posterAPI
	logf func(format string, args ...any)
	// positionChecked guards the one-shot self-check described in diffpos.go.
	positionChecked bool
}

func newPoster(api posterAPI, logf func(string, ...any)) *poster {
	return &poster{api: api, logf: logf}
}

// Post turns a review into comments on the merge request.
//
// Findings are annotated on the line they are about whenever the diff gives a
// position for it. Everything else -- findings in files the diff does not
// cover, findings on lines outside the hunks, and findings Gitee refused --
// goes into a single summary comment that is rewritten on each run rather than
// posted again.
func (p *poster) Post(ctx context.Context, job *reviewJob, commitID string, report *ocrReport, options postOptions) (*postResult, error) {
	result := &postResult{}

	files, err := p.api.ListPRFiles(ctx, job.Owner, job.Repo, job.Number)
	if err != nil {
		return nil, fmt.Errorf("list changed files: %w", err)
	}
	positions := make(map[string]map[int]LinePosition, len(files))
	for _, f := range files {
		positions[f.Filename] = buildLinePositions(f.Patch.Diff)
	}

	existing, err := p.api.ListPRComments(ctx, job.Owner, job.Repo, job.Number)
	if err != nil {
		return nil, fmt.Errorf("list existing comments: %w", err)
	}
	summaryID, alreadyPosted := scanComments(existing)
	result.SummaryID = summaryID

	// Once the position model is known to be wrong, the remaining findings are
	// listed rather than annotated: a comment on the wrong line is worse than
	// no comment, because the reader trusts it.
	inlineOK := true

	for _, comment := range report.Comments {
		if alreadyPosted[findingMarker(comment)] {
			result.Skipped++
			continue
		}
		if !inlineOK {
			result.Folded = append(result.Folded, comment)
			continue
		}
		anchor, ok := anchorFor(positions, comment)
		if !ok {
			result.Folded = append(result.Folded, comment)
			continue
		}

		created, err := p.api.CreateLineComment(ctx, job.Owner, job.Repo, job.Number,
			renderFinding(comment), commitID, comment.Path, anchor.Position)
		if err != nil {
			// One rejected line must not cost the other findings, which are
			// still perfectly postable.
			p.logf("inline comment on %s:%d was rejected: %v", comment.Path, anchor.Line, err)
			result.Folded = append(result.Folded, comment)
			continue
		}
		result.Inline++

		if !p.positionChecked {
			// The check needs a line to compare against, and the response does
			// not always carry one. Marking it done anyway would spend the whole
			// review's one opportunity to notice a wrong position model on a
			// response that said nothing, leaving every later comment posted
			// inline on a position that was never confirmed.
			got, known := createdLine(created)
			if !known {
				continue
			}
			p.positionChecked = true
			if got != anchor.Line {
				p.logf("position model looks wrong: asked for %s:%d (position %d) and Gitee filed it on line %d; "+
					"listing the rest of this review instead of annotating it",
					comment.Path, anchor.Line, anchor.Position, got)
				inlineOK = false
			}
		}
	}

	summaryID, err = p.writeSummary(ctx, job, commitID, report, summaryID, result, options)
	if err != nil {
		return result, err
	}
	result.SummaryID = summaryID
	return result, nil
}

// anchorFor finds the diff position for a finding, or reports that the diff
// cannot carry it.
func anchorFor(positions map[string]map[int]LinePosition, comment ocrComment) (LinePosition, bool) {
	filePositions, ok := positions[comment.Path]
	if !ok {
		// A finding in a file the diff does not mention. ocr occasionally
		// reviews a file the API withheld a patch for; there is no position to
		// compute, but the finding is still worth showing.
		return LinePosition{}, false
	}
	return anchorPosition(filePositions, comment.StartLine, comment.EndLine)
}

// writeSummary creates the sticky summary, or rewrites the one already there.
func (p *poster) writeSummary(ctx context.Context, job *reviewJob, commitID string, report *ocrReport,
	summaryID int64, result *postResult, options postOptions) (int64, error) {
	body := renderSummary(summaryInput{
		Job:      job,
		CommitID: commitID,
		Report:   report,
		Result:   result,
		Options:  options,
	})

	if summaryID != 0 {
		if err := p.api.UpdateComment(ctx, job.Owner, job.Repo, summaryID, body); err != nil {
			return summaryID, fmt.Errorf("update summary comment: %w", err)
		}
		return summaryID, nil
	}
	created, err := p.api.CreateSummaryComment(ctx, job.Owner, job.Repo, job.Number, body)
	if err != nil {
		return 0, fmt.Errorf("create summary comment: %w", err)
	}
	return created.ID, nil
}

// scanComments finds the sticky summary and collects the markers of the inline
// comments already on the merge request.
func scanComments(comments []pullRequestComment) (summaryID int64, posted map[string]bool) {
	posted = map[string]bool{}
	for _, c := range comments {
		if summaryID == 0 && strings.Contains(c.Body, summaryMarker) {
			summaryID = c.ID
		}
		for _, marker := range markersIn(c.Body) {
			posted[marker] = true
		}
	}
	return summaryID, posted
}

// markersIn pulls the inline markers out of a comment body.
func markersIn(body string) []string {
	var found []string
	rest := body
	for {
		start := strings.Index(rest, inlinePrefix)
		if start < 0 {
			return found
		}
		rest = rest[start:]
		end := strings.Index(rest, "-->")
		if end < 0 {
			return found
		}
		found = append(found, rest[:end+len("-->")])
		rest = rest[end+len("-->"):]
	}
}

// findingMarker identifies a finding by its content rather than by its position
// in the review, so the same finding is recognised even if ocr reports it in a
// different order next time.
func findingMarker(comment ocrComment) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%s",
		comment.Path, comment.StartLine, comment.EndLine, comment.Content)))
	return inlinePrefix + hex.EncodeToString(digest[:])[:12] + " -->"
}

// createdLine reads back the line Gitee says it filed a comment on. It is
// absent in some responses, and a dry run never supplies one.
func createdLine(created *pullRequestComment) (int, bool) {
	if created == nil || created.NewLine == nil {
		return 0, false
	}
	return *created.NewLine, true
}

// renderFinding formats one annotation. The marker goes first so that a
// re-delivered webhook recognises the comment before reading any of it.
func renderFinding(comment ocrComment) string {
	var b strings.Builder
	b.WriteString(findingMarker(comment))
	b.WriteString("\n")

	if header := findingHeader(comment); header != "" {
		b.WriteString(header)
		b.WriteString("\n\n")
	}
	b.WriteString(strings.TrimSpace(comment.Content))
	b.WriteString("\n")

	if code := strings.TrimSpace(comment.SuggestionCode); code != "" {
		b.WriteString("\n**Suggested change:**\n\n")
		writeFence(&b, comment.Path, code)
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// findingHeader is the one-line label above a finding: what kind of problem it
// is and how serious, both omitted when ocr did not say.
func findingHeader(comment ocrComment) string {
	var parts []string
	if category := strings.TrimSpace(comment.Category); category != "" {
		parts = append(parts, "**"+category+"**")
	}
	if severity := strings.TrimSpace(comment.Severity); severity != "" {
		parts = append(parts, "severity: "+severity)
	}
	return strings.Join(parts, " | ")
}

func writeFence(b *strings.Builder, filePath, code string) {
	b.WriteString("```")
	if language := fenceLanguage(filePath); language != "" {
		b.WriteString(language)
	}
	b.WriteString("\n")
	b.WriteString(code)
	b.WriteString("\n```\n")
}

// fenceLanguage labels a code block so the suggestion is highlighted. An
// unknown extension simply gets an unlabelled block.
func fenceLanguage(filePath string) string {
	return fenceLanguages[strings.ToLower(path.Ext(filePath))]
}

var fenceLanguages = map[string]string{
	".c": "c", ".cc": "cpp", ".cpp": "cpp", ".cs": "csharp", ".css": "css",
	".go": "go", ".h": "c", ".hpp": "cpp", ".html": "html", ".java": "java",
	".js": "javascript", ".json": "json", ".jsx": "jsx", ".kt": "kotlin",
	".kts": "kotlin", ".md": "markdown", ".mjs": "javascript", ".php": "php",
	".py": "python", ".rb": "ruby", ".rs": "rust", ".sh": "bash", ".sql": "sql",
	".swift": "swift", ".ts": "typescript", ".tsx": "tsx", ".vue": "vue",
	".xml": "xml", ".yaml": "yaml", ".yml": "yaml",
}

type summaryInput struct {
	Job      *reviewJob
	CommitID string
	Report   *ocrReport
	Result   *postResult
	Options  postOptions
}

// renderSummary writes the one comment that is rewritten in place on every run
// rather than posted again.
func renderSummary(in summaryInput) string {
	var b strings.Builder
	b.WriteString(summaryMarker)
	b.WriteString("\n## Automated review\n\n")
	fmt.Fprintf(&b, "Revision `%s` of `%s`.\n\n", shortSHA(in.CommitID), in.Job.String())

	report := in.Report
	fmt.Fprintf(&b, "- Annotated in the diff: %d\n", in.Result.Inline)
	if len(in.Result.Folded) > 0 {
		fmt.Fprintf(&b, "- Listed below: %d\n", len(in.Result.Folded))
	}
	if in.Result.Skipped > 0 {
		fmt.Fprintf(&b, "- Already reported: %d\n", in.Result.Skipped)
	}
	if in.Options.Dropped > 0 {
		fmt.Fprintf(&b, "- Removed as contradicted by a pinned dependency: %d\n", in.Options.Dropped)
	}
	if report != nil && report.Summary != nil {
		// ocr's own account of the run, so a reader can tell a review of four
		// files from one of four hundred without opening the job log.
		fmt.Fprintf(&b, "- Reviewed: %d files in %s\n", report.Summary.FilesReviewed, report.Summary.Elapsed)
	}
	if report != nil && report.ProjectSummary != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimSpace(report.ProjectSummary))
	}

	writeFolded(&b, in.Result.Folded)
	writeWarnings(&b, report)
	writeNotes(&b, in.Options.Notes)

	b.WriteString("\n<sub>Posted by open-code-review. This comment is rewritten on each push; " +
		"the annotations above are posted once per revision.</sub>\n")
	return b.String()
}

// writeFolded lists the findings that could not be attached to a line.
func writeFolded(b *strings.Builder, folded []ocrComment) {
	if len(folded) == 0 {
		return
	}
	b.WriteString("\n### Findings outside the diff\n\n")
	for _, comment := range folded {
		fmt.Fprintf(b, "#### `%s`\n\n", commentLocation(comment))
		if header := findingHeader(comment); header != "" {
			b.WriteString(header)
			b.WriteString("\n\n")
		}
		b.WriteString(strings.TrimSpace(comment.Content))
		b.WriteString("\n")
		if code := strings.TrimSpace(comment.SuggestionCode); code != "" {
			b.WriteString("\n**Suggested change:**\n\n")
			writeFence(b, comment.Path, code)
		}
		b.WriteString("\n")
	}
}

func writeWarnings(b *strings.Builder, report *ocrReport) {
	if report == nil || len(report.Warnings) == 0 {
		return
	}
	b.WriteString("\n### Warnings\n\n")
	for _, warning := range report.Warnings {
		fmt.Fprintf(b, "- `%s`", warning.File)
		if warning.Type != "" {
			fmt.Fprintf(b, " (%s)", warning.Type)
		}
		fmt.Fprintf(b, ": %s\n", warning.Message)
	}
}

// writeNotes carries whatever the verification round could not settle. It is
// deliberately separate from the findings: a note never rewrites a finding, it
// only tells the reader that one was left unconfirmed.
func writeNotes(b *strings.Builder, notes []string) {
	if len(notes) == 0 {
		return
	}
	b.WriteString("\n### Unconfirmed\n\n")
	for _, note := range notes {
		fmt.Fprintf(b, "- %s\n", note)
	}
}

// commentLocation renders a finding's position the way a reader would look it
// up. Line 0 means ocr did not say, which is itself worth showing.
func commentLocation(comment ocrComment) string {
	switch {
	case comment.StartLine <= 0:
		return comment.Path
	case comment.EndLine > comment.StartLine:
		return fmt.Sprintf("%s:%d-%d", comment.Path, comment.StartLine, comment.EndLine)
	default:
		return fmt.Sprintf("%s:%d", comment.Path, comment.StartLine)
	}
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
