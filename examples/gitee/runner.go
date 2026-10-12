// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ocrComment mirrors one entry of the review JSON. The field names are the
// ones ocr actually emits, read off a real run rather than from the docs.
type ocrComment struct {
	Path           string `json:"path"`
	Content        string `json:"content"`
	SuggestionCode string `json:"suggestion_code"`
	ExistingCode   string `json:"existing_code"`
	StartLine      int    `json:"start_line"`
	EndLine        int    `json:"end_line"`
	Thinking       string `json:"thinking"`
	Category       string `json:"category"`
	Severity       string `json:"severity"`
}

type ocrWarning struct {
	File    string `json:"file"`
	Type    string `json:"type"`
	Message string `json:"message"`
}

type ocrManifest struct {
	TerminalState string `json:"terminal_state"`
	Input         struct {
		ResolvedBase string `json:"resolved_base"`
		ResolvedHead string `json:"resolved_head"`
	} `json:"input"`
}

// ocrSummary mirrors the `summary` object ocr emits. It is an object, not the
// prose: decoding it as a string fails the whole document, because
// encoding/json refuses a type mismatch rather than skipping the field.
type ocrSummary struct {
	FilesReviewed int64  `json:"files_reviewed"`
	Comments      int64  `json:"comments"`
	TotalTokens   int64  `json:"total_tokens"`
	Elapsed       string `json:"elapsed"`
}

// ocrReport is the review result. Only the fields the poster and the verifier
// need are decoded; the rest of the document is left alone.
type ocrReport struct {
	Status   string       `json:"status"`
	Message  string       `json:"message"`
	Summary  *ocrSummary  `json:"summary"`
	Comments []ocrComment `json:"comments"`
	Warnings []ocrWarning `json:"warnings"`
	// ProjectSummary is the prose overview, which is where ocr's own account of
	// the change lives. It is a separate field from Summary and is often absent.
	ProjectSummary string       `json:"project_summary"`
	Manifest       *ocrManifest `json:"manifest"`
}

// ReviewedHead is the commit ocr actually reviewed. It is preferred over the
// head sha from the webhook because ocr records what it resolved, and inline
// comments have to be pinned to the commit the diff was computed from.
func (r *ocrReport) ReviewedHead(fallback string) string {
	if r.Manifest != nil && strings.TrimSpace(r.Manifest.Input.ResolvedHead) != "" {
		return strings.TrimSpace(r.Manifest.Input.ResolvedHead)
	}
	return fallback
}

// stateComplete is ocr's terminal state for a run that covered everything it
// selected. The others -- partial, failed, skipped -- all mean some of the
// review did not happen.
const stateComplete = "complete"

// noteIncomplete adds a warning the reader of the merge request will see. It
// carries no upstream error text: the summary comment is public, and ocr's
// diagnostics can name the configured gateway.
func (r *ocrReport) noteIncomplete(message string) {
	r.Warnings = append(r.Warnings, ocrWarning{Type: "service", Message: message})
}

// noteTerminalState reports a run that did not complete, which is how a review
// that lost file groups to a failure or a budget becomes visible. Posting the
// partial findings is right; posting them as though they were the whole review
// is not.
func (r *ocrReport) noteTerminalState() {
	if r.Manifest == nil {
		return
	}
	if state := strings.TrimSpace(r.Manifest.TerminalState); state != "" && state != stateComplete {
		r.noteIncomplete(fmt.Sprintf("ocr finished with terminal state %q, so this review may be incomplete", state))
	}
}

// commandRunner is the seam between the runner and the operating system, so
// the argument construction can be asserted without a git repository or an
// ocr binary.
type commandRunner interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) (stdout []byte, err error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	// Stdout is captured through an explicit buffer rather than with
	// cmd.Output(), which discards what was written when the command exits
	// non-zero. ocr writes its findings before it can fail, so that output is
	// the difference between a partial review and no review at all.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// runner fetches the two commits a review range needs and drives ocr.
type runner struct {
	cfg  *Config
	exec commandRunner
	// cloneBase is the scheme and host git fetches from, derived from the API
	// base so the instance reviews are read from and the instance they are
	// posted to cannot disagree; a test points it at a local repository.
	cloneBase string
	// askPass is where the credential helper is written. Prepare sets it to a
	// path inside that run's own credential directory: a runner belongs to one
	// review, so the field is written once and read by every git call in it.
	askPass string
	logf    func(format string, args ...any)
}

func newRunner(cfg *Config) *runner {
	return &runner{
		cfg:       cfg,
		exec:      execRunner{},
		cloneBase: gitBaseFromAPI(cfg.GiteeAPIBase),
		logf:      log.Printf,
	}
}

// defaultCloneBase is where git fetches from when the API base names no host,
// which is also what the default API base derives to.
const defaultCloneBase = "https://gitee.com"

// gitBaseFromAPI reduces GITEE_API_BASE to the scheme and host git fetches from.
//
// Only Gitee's REST API lives under the /api/v5 path; its repositories do not,
// and on a self-hosted instance the two are the same host. Deriving one from the
// other is what makes the README's "change GITEE_API_BASE for Gitee Enterprise"
// true -- otherwise a self-hosted instance has its comments posted to the right
// server and its source fetched from gitee.com.
func gitBaseFromAPI(apiBase string) string {
	parsed, err := url.Parse(strings.TrimSpace(apiBase))
	if err != nil || parsed.Host == "" {
		return defaultCloneBase
	}
	return parsed.Scheme + "://" + parsed.Host
}

// workRoot is where checkouts and credentials are created.
func (r *runner) workRoot() string {
	if r.cfg.WorkRoot != "" {
		return r.cfg.WorkRoot
	}
	return os.TempDir()
}

// repositoryURL is the fetch URL. Credentials never go in here: they would end
// up in .git/config and in the process arguments, both readable by anything
// else on the machine. They are supplied through GIT_ASKPASS instead.
func (r *runner) repositoryURL(job *reviewJob) string {
	return fmt.Sprintf("%s/%s/%s.git", strings.TrimRight(r.cloneBase, "/"), job.Owner, job.Repo)
}

// prepareRepo creates a working copy containing both commits of the range.
//
// Nothing from the merge request is executed. A checkout only writes files;
// the install and build steps that would run an attacker's code are
// deliberately absent, which is also why the verifier reads lock files out of
// git objects rather than from an installed dependency tree.
func (r *runner) prepareRepo(ctx context.Context, job *reviewJob) (string, error) {
	dir, err := os.MkdirTemp(r.workRoot(), "ocr-gitee-")
	if err != nil {
		return "", fmt.Errorf("create working directory: %w", err)
	}

	env := r.gitEnv()
	git := func(args ...string) ([]byte, error) {
		return r.exec.Run(ctx, dir, env, "git", args...)
	}

	if _, err := git("init", "--quiet"); err != nil {
		return dir, fmt.Errorf("git init: %w", err)
	}
	if _, err := git("remote", "add", "origin", r.repositoryURL(job)); err != nil {
		return dir, fmt.Errorf("git remote add: %w", err)
	}

	// Fetching the branch heads brings both commits whenever they are
	// reachable from a branch, which is the normal case for a merge request.
	if _, err := git("fetch", "--quiet", "--no-tags", "origin",
		"+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return dir, fmt.Errorf("git fetch branches: %w", err)
	}

	for _, sha := range []string{job.BaseSha, job.HeadSha} {
		if r.hasCommit(ctx, dir, env, sha) {
			continue
		}
		// The commit is not on any branch: a force-push, or a range the default
		// fetch depth did not carry. Asking for it by id is the only way left,
		// and it fails cleanly when the server refuses.
		if _, err := git("fetch", "--quiet", "--no-tags", "origin", sha); err != nil {
			return dir, fmt.Errorf("commit %s is not reachable: %w", sha, err)
		}
		if !r.hasCommit(ctx, dir, env, sha) {
			return dir, fmt.Errorf("commit %s is still missing after fetching it", sha)
		}
	}

	// ocr resolves file contents from git objects, but a working tree that
	// matches the reviewed commit keeps its file tools honest for anything
	// that reads the filesystem directly.
	if _, err := git("-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", job.HeadSha); err != nil {
		return dir, fmt.Errorf("git checkout: %w", err)
	}
	return dir, nil
}

func (r *runner) hasCommit(ctx context.Context, dir string, env []string, sha string) bool {
	_, err := r.exec.Run(ctx, dir, env, "git", "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// gitEnv supplies credentials to git without putting them on the command line.
// GIT_ASKPASS is invoked by git whenever it needs a username or password, so
// the token reaches git through a pipe and never through argv or a config file.
func (r *runner) gitEnv() []string {
	env := append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GITEE_GIT_USERNAME="+valueOr(r.cfg.GitUsername, "oauth2"),
		"GITEE_GIT_TOKEN="+r.cfg.GiteeToken,
		"GIT_ASKPASS="+r.askPass,
	)
	return env
}

// askPassScript answers git's credential prompts from the environment. Gitee
// authenticates a personal access token as the password, with the account name
// as the username.
var askPassScript = "#!/bin/sh\n" +
	"case \"$1\" in\n" +
	"  *[Uu]sername*) printf '%s' \"$GITEE_GIT_USERNAME\" ;;\n" +
	"  *) printf '%s' \"$GITEE_GIT_TOKEN\" ;;\n" +
	"esac\n"

// ensureAskPass writes the credential helper. A failure is not fatal here: git
// still fails loudly at fetch time if the file is missing, and public
// repositories need no credentials at all.
func (r *runner) ensureAskPass() error {
	if current, err := os.ReadFile(r.askPass); err == nil && string(current) == askPassScript {
		return nil
	}
	if err := os.WriteFile(r.askPass, []byte(askPassScript), 0o700); err != nil {
		return fmt.Errorf("write askpass helper: %w", err)
	}
	return nil
}

// workspace is one checked-out revision. It stays alive for as long as anything
// needs to read from it, which is why the review and the dependency
// verification share one rather than each preparing their own.
type workspace struct {
	runner *runner
	job    *reviewJob
	dir    string
	// credentials holds the git credential helper for this run. It is a
	// directory of its own so that two reviews running at once cannot write to
	// the same helper file, and so nothing else on the machine can pre-create
	// the path.
	credentials string
}

// Dir is the checked-out tree.
func (w *workspace) Dir() string { return w.dir }

func (w *workspace) Close() error {
	err := os.RemoveAll(w.dir)
	if w.credentials != "" {
		err = errors.Join(err, os.RemoveAll(w.credentials))
	}
	return err
}

// ReadFile reads a file out of the reviewed revision.
//
// The working tree was checked out at the head commit, so this is the head
// revision's content and no git call is needed. Reading a file executes
// nothing: this is how the verifier learns which dependency versions the
// revision pins without ever running an install, which would execute whatever
// scripts the merge request brought with it.
func (w *workspace) ReadFile(name string) ([]byte, error) {
	full := filepath.Join(w.dir, name)
	// The names come from the verifier's own list, but a path that escaped the
	// working tree would be a bug worth failing on rather than reading.
	if !strings.HasPrefix(full, w.dir+string(os.PathSeparator)) {
		return nil, fmt.Errorf("%s is outside the working tree", name)
	}
	// A repository can contain a symlink named after a manifest -- git recreates
	// symlinks on checkout -- and reading through it would hand back a file from
	// anywhere on the machine, whose contents then travel to the model gateway
	// as evidence. Only regular files are read.
	info, err := os.Lstat(full)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file in the working tree", name)
	}
	return os.ReadFile(full)
}

// Prepare checks out both commits of the range and returns a workspace over the
// head revision.
func (r *runner) Prepare(ctx context.Context, job *reviewJob) (*workspace, error) {
	// The credential helper is written before the first git call, into a
	// directory created for this run alone.
	credentials, err := os.MkdirTemp(r.workRoot(), "ocr-gitee-cred-")
	if err != nil {
		return nil, fmt.Errorf("create credential directory: %w", err)
	}
	r.askPass = filepath.Join(credentials, "askpass.sh")
	if err := r.ensureAskPass(); err != nil {
		_ = os.RemoveAll(credentials)
		return nil, err
	}

	dir, err := r.prepareRepo(ctx, job)
	if err != nil {
		// prepareRepo creates the directory before it can fail, and the caller
		// gets no workspace to close, so it is removed here rather than left
		// behind for every failed review to accumulate.
		_ = os.RemoveAll(dir)
		_ = os.RemoveAll(credentials)
		return nil, err
	}
	return &workspace{runner: r, job: job, dir: dir, credentials: credentials}, nil
}

// Run fetches the range and runs ocr over it, returning the parsed report.
func (w *workspace) Run(ctx context.Context) (*ocrReport, error) {
	// The same invocation every other platform integration uses, so a change
	// in ocr's behaviour shows up here the way it does everywhere else.
	args := []string{
		"review",
		"--from", w.job.BaseSha,
		"--to", w.job.HeadSha,
		"--format", "json",
		"--audience", "agent",
		"--timeout", fmt.Sprintf("%d", w.runner.cfg.ReviewTimeoutMin),
		"--concurrency", fmt.Sprintf("%d", w.runner.cfg.Concurrency),
	}
	if strings.TrimSpace(w.job.Title) != "" {
		// The title is the author's own statement of intent, and it is the
		// cheapest context available for judging whether a change is correct.
		args = append(args, "--background", w.job.Title)
	}

	stdout, err := w.runner.exec.Run(ctx, w.dir, w.runner.cfg.LLMEnv(os.Environ()), w.runner.cfg.OcrBin, args...)
	if err != nil {
		// ocr writes its findings as JSON on stdout; a non-zero exit with a
		// parseable document still means partial results worth posting. The
		// failure itself is not discarded: the detail goes to the log, and a
		// warning the reader sees goes into the report, so a truncated review is
		// not posted as though it were the whole one.
		if report, parseErr := parseReport(stdout); parseErr == nil && len(report.Comments) > 0 {
			w.runner.logf("ocr exited non-zero over %s: %v", shortSHA(w.job.HeadSha), err)
			report.noteIncomplete("ocr exited with an error partway through this review, so the findings below may be incomplete")
			return report, nil
		}
		return nil, fmt.Errorf("ocr review: %w", err)
	}

	report, err := parseReport(stdout)
	if err != nil {
		return nil, err
	}
	report.noteTerminalState()
	return report, nil
}

// parseReport decodes ocr's stdout. The document is the only thing on stdout
// with --audience agent, but a stray progress line before it would otherwise
// be fatal, so the first '{' is used as the start when a plain decode fails.
func parseReport(stdout []byte) (*ocrReport, error) {
	trimmed := strings.TrimSpace(string(stdout))
	if trimmed == "" {
		return nil, errors.New("ocr produced no output on stdout")
	}
	var report ocrReport
	if err := json.Unmarshal([]byte(trimmed), &report); err == nil {
		return &report, nil
	}
	if start := strings.Index(trimmed, "{"); start > 0 {
		if err := json.Unmarshal([]byte(trimmed[start:]), &report); err == nil {
			return &report, nil
		}
	}
	return nil, fmt.Errorf("ocr output is not valid JSON (first 200 bytes: %s)", preview(trimmed, 200))
}

func preview(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
