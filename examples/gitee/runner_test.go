// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeExec records every command and answers from a scripted table, so the
// argument construction can be asserted without git or ocr installed.
type fakeExec struct {
	calls []fakeCall
	// fail answers every matching call with an error.
	fail map[string]error
	// failOnce answers only the first matching call with an error, which is how
	// a recovery after a fallback is scripted.
	failOnce map[string]error
	outputs  map[string][]byte
}

type fakeCall struct {
	Dir  string
	Env  []string
	Name string
	Args []string
}

func (f *fakeExec) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, fakeCall{Dir: dir, Env: env, Name: name, Args: args})
	key := name + " " + strings.Join(args, " ")
	// A failing command still yields whatever it wrote to stdout, which is how
	// exec.Cmd behaves and what lets a partial report survive a non-zero exit.
	if err, ok := f.failOnce[key]; ok {
		delete(f.failOnce, key)
		return f.outputs[key], err
	}
	if err := f.fail[key]; err != nil {
		return f.outputs[key], err
	}
	if out, ok := f.outputs[key]; ok {
		return out, nil
	}
	return nil, nil
}

// has reports whether any recorded call matches the given command prefix.
func (f *fakeExec) has(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c.Name+" "+strings.Join(c.Args, " "), prefix) {
			return true
		}
	}
	return false
}

func (f *fakeExec) find(prefix string) (fakeCall, bool) {
	for _, c := range f.calls {
		if strings.HasPrefix(c.Name+" "+strings.Join(c.Args, " "), prefix) {
			return c, true
		}
	}
	return fakeCall{}, false
}

func testRunner(t *testing.T) (*runner, *fakeExec) {
	t.Helper()
	fake := &fakeExec{
		fail:     map[string]error{},
		failOnce: map[string]error{},
		outputs:  map[string][]byte{},
	}
	return &runner{
		cfg: &Config{
			GiteeToken:       "test-token",
			GitUsername:      "someone",
			LLMURL:           "https://llm.example/v1",
			LLMToken:         "llm-token",
			LLMModel:         "a-model",
			OcrBin:           "ocr",
			ReviewTimeoutMin: 15,
			Concurrency:      4,
			WorkRoot:         t.TempDir(),
		},
		exec:      fake,
		cloneBase: "https://gitee.example",
		askPass:   filepath.Join(t.TempDir(), "askpass.sh"),
		logf:      func(string, ...any) {},
	}, fake
}

func TestRepositoryURL(t *testing.T) {
	r, _ := testRunner(t)
	got := r.repositoryURL(&reviewJob{Owner: "acme", Repo: "widget"})
	if got != "https://gitee.example/acme/widget.git" {
		t.Fatalf("repositoryURL = %q", got)
	}
	// Credentials must not be embedded: this string reaches .git/config.
	if strings.Contains(got, "test-token") {
		t.Fatal("the repository URL must not carry the token")
	}
}

func TestPrepareRepoFetchesBothCommits(t *testing.T) {
	r, fake := testRunner(t)
	job := &reviewJob{Owner: "acme", Repo: "widget", BaseSha: "base1", HeadSha: "head1"}

	dir, err := r.prepareRepo(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dir == "" {
		t.Fatal("want a working directory")
	}

	for _, want := range []string{
		"git init",
		"git remote add origin https://gitee.example/acme/widget.git",
		"git fetch --quiet --no-tags origin +refs/heads/*:refs/remotes/origin/*",
		"git -c advice.detachedHead=false checkout --quiet --detach head1",
	} {
		if !fake.has(want) {
			t.Errorf("missing call: %s\ngot: %v", want, fake.calls)
		}
	}
}

// Credentials reach git through the environment, never through argv, which any
// other process on the machine can read.
func TestGitCredentialsStayOutOfTheArguments(t *testing.T) {
	r, fake := testRunner(t)
	job := &reviewJob{Owner: "acme", Repo: "widget", BaseSha: "b", HeadSha: "h"}
	if _, err := r.prepareRepo(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, call := range fake.calls {
		joined := call.Name + " " + strings.Join(call.Args, " ")
		if strings.Contains(joined, "test-token") {
			t.Fatalf("the token leaked into the command line: %s", joined)
		}
	}
	call, _ := fake.find("git fetch")
	var sawToken, sawAskPass bool
	for _, entry := range call.Env {
		if entry == "GITEE_GIT_TOKEN=test-token" {
			sawToken = true
		}
		if strings.HasPrefix(entry, "GIT_ASKPASS=") {
			sawAskPass = true
		}
	}
	if !sawToken || !sawAskPass {
		t.Fatalf("git should be handed the token and an askpass helper, got env %v", call.Env)
	}
	if !fake.has("git fetch") {
		t.Fatal("no fetch recorded")
	}
}

// A commit that no branch reaches still has to be fetched by id, which is the
// case after a force-push.
func TestPrepareRepoFetchesMissingCommitsByID(t *testing.T) {
	r, fake := testRunner(t)
	// The first cat-file fails, so the runner falls back to fetching by id;
	// the check that follows succeeds, and only the head needs this.
	fake.failOnce["git cat-file -e head1^{commit}"] = errors.New("missing")
	job := &reviewJob{Owner: "acme", Repo: "widget", BaseSha: "base1", HeadSha: "head1"}

	if _, err := r.prepareRepo(context.Background(), job); err != nil {
		t.Fatalf("unexpected error: %v\ncalls: %v", err, fake.calls)
	}
	if !fake.has("git fetch --quiet --no-tags origin head1") {
		t.Fatalf("a missing commit should be fetched by id, calls: %v", fake.calls)
	}
	// The commit that the branch fetch already delivered must not be re-fetched.
	if fake.has("git fetch --quiet --no-tags origin base1") {
		t.Errorf("a reachable commit should not be fetched by id, calls: %v", fake.calls)
	}
}

func TestPrepareRepoFailsWhenACommitStaysMissing(t *testing.T) {
	r, fake := testRunner(t)
	fake.fail["git cat-file -e base1^{commit}"] = errors.New("missing")
	job := &reviewJob{Owner: "acme", Repo: "widget", BaseSha: "base1", HeadSha: "head1"}
	_, err := r.prepareRepo(context.Background(), job)
	if err == nil || !strings.Contains(err.Error(), "base1") {
		t.Fatalf("want an error naming the commit, got: %v", err)
	}
}

func TestPrepareRepoSurfacesFetchFailure(t *testing.T) {
	r, fake := testRunner(t)
	fake.fail["git fetch --quiet --no-tags origin +refs/heads/*:refs/remotes/origin/*"] = errors.New("no network")
	job := &reviewJob{Owner: "acme", Repo: "widget", BaseSha: "b", HeadSha: "h"}
	if _, err := r.prepareRepo(context.Background(), job); err == nil {
		t.Fatal("want an error")
	}
}

// runReview prepares the workspace and runs ocr, which is what the service does
// in two steps so the verifier can keep reading from the checkout afterwards.
func runReview(t *testing.T, r *runner, job *reviewJob) (*ocrReport, error) {
	t.Helper()
	ws, err := r.Prepare(context.Background(), job)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws.Run(context.Background())
}

func TestReviewBuildsTheExpectedCommand(t *testing.T) {
	r, fake := testRunner(t)
	report := `{"status":"ok","comments":[{"path":"a.ts","content":"c","start_line":1,"end_line":2}],
		"manifest":{"terminal_state":"complete","input":{"resolved_head":"head1","resolved_base":"base1"}}}`
	fake.outputs["ocr review --from base1 --to head1 --format json --audience agent --timeout 15 --concurrency 4 --background Add the thing"] = []byte(report)

	job := &reviewJob{Owner: "acme", Repo: "widget", BaseSha: "base1", HeadSha: "head1", Title: "Add the thing"}
	got, err := runReview(t, r, job)
	if err != nil {
		t.Fatalf("unexpected error: %v\ncalls: %v", err, fake.calls)
	}
	if len(got.Comments) != 1 || got.Comments[0].Path != "a.ts" {
		t.Fatalf("report not parsed: %+v", got)
	}
	if got.ReviewedHead("fallback") != "head1" {
		t.Errorf("ReviewedHead = %q", got.ReviewedHead("fallback"))
	}
	// The subprocess gets the LLM credentials and nothing from the Gitee side.
	call, ok := fake.find("ocr review")
	if !ok {
		t.Fatal("no ocr call recorded")
	}
	joined := strings.Join(call.Env, "\n")
	if !strings.Contains(joined, "OCR_LLM_URL=") {
		t.Error("ocr should receive the LLM credentials")
	}
}

func TestReviewOmitsBackgroundWhenTheTitleIsEmpty(t *testing.T) {
	r, fake := testRunner(t)
	fake.outputs["ocr review --from base1 --to head1 --format json --audience agent --timeout 15 --concurrency 4"] = []byte(`{"status":"ok"}`)
	job := &reviewJob{Owner: "acme", Repo: "widget", BaseSha: "base1", HeadSha: "head1"}
	if _, err := runReview(t, r, job); err != nil {
		t.Fatalf("unexpected error: %v\ncalls: %v", err, fake.calls)
	}
	if fake.has("ocr review --from base1 --to head1 --format json --audience agent --timeout 15 --concurrency 4 --background") {
		t.Error("an empty title should not produce a --background flag")
	}
}

// ocr exits non-zero when every group failed, but partial findings are still
// worth posting; discarding them would throw away real work.
func TestReviewKeepsPartialResultsFromAFailingRun(t *testing.T) {
	r, fake := testRunner(t)
	key := "ocr review --from base1 --to head1 --format json --audience agent --timeout 15 --concurrency 4"
	fake.fail[key] = errors.New("exit status 1")
	fake.outputs[key] = []byte(`{"status":"partial","comments":[{"path":"a.ts","content":"c"}]}`)

	job := &reviewJob{Owner: "acme", Repo: "widget", BaseSha: "base1", HeadSha: "head1"}
	got, err := runReview(t, r, job)
	if err != nil {
		t.Fatalf("partial results should survive a non-zero exit: %v", err)
	}
	if len(got.Comments) != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestReviewFailsWhenNothingCanBeSalvaged(t *testing.T) {
	r, fake := testRunner(t)
	key := "ocr review --from base1 --to head1 --format json --audience agent --timeout 15 --concurrency 4"
	fake.fail[key] = errors.New("exit status 1")
	fake.outputs[key] = []byte(`not json at all`)

	job := &reviewJob{Owner: "acme", Repo: "widget", BaseSha: "base1", HeadSha: "head1"}
	if _, err := runReview(t, r, job); err == nil {
		t.Fatal("want an error")
	}
}

// Partial findings still post, and the review no longer looks like a whole one:
// a run that lost file groups to a failure reads the same as a complete run
// otherwise, which is the failure mode the verification round also refuses.
func TestReviewMarksAFailedRunIncomplete(t *testing.T) {
	r, fake := testRunner(t)
	key := "ocr review --from base1 --to head1 --format json --audience agent --timeout 15 --concurrency 4"
	fake.fail[key] = errors.New("exit status 1: dial tcp: the gateway is unreachable")
	fake.outputs[key] = []byte(`{"status":"partial","comments":[{"path":"a.ts","content":"c"}]}`)

	job := &reviewJob{Owner: "acme", Repo: "widget", BaseSha: "base1", HeadSha: "head1"}
	got, err := runReview(t, r, job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0].Message, "incomplete") {
		t.Fatalf("warnings = %+v, want one saying the review is incomplete", got.Warnings)
	}
	// The warning is posted on the merge request, and the failure text can name
	// the configured gateway. It belongs in the log instead.
	if strings.Contains(got.Warnings[0].Message, "gateway") {
		t.Errorf("the reader-facing warning quotes the failure: %q", got.Warnings[0].Message)
	}
}

func TestReviewNotesATerminalStateThatIsNotComplete(t *testing.T) {
	key := "ocr review --from base1 --to head1 --format json --audience agent --timeout 15 --concurrency 4"
	cases := map[string]int{
		`{"manifest":{"terminal_state":"complete"}}`: 0,
		`{"manifest":{"terminal_state":"partial"}}`:  1,
		`{"manifest":{"terminal_state":"failed"}}`:   1,
		`{"manifest":{"terminal_state":"skipped"}}`:  1,
		`{"manifest":{}}`: 0,
		`{"status":"ok"}`: 0,
	}
	for document, wantWarnings := range cases {
		t.Run(document, func(t *testing.T) {
			r, fake := testRunner(t)
			fake.outputs[key] = []byte(document)

			job := &reviewJob{Owner: "acme", Repo: "widget", BaseSha: "base1", HeadSha: "head1"}
			got, err := runReview(t, r, job)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got.Warnings) != wantWarnings {
				t.Fatalf("warnings = %+v, want %d", got.Warnings, wantWarnings)
			}
		})
	}
}

func TestParseReport(t *testing.T) {
	t.Run("clean document", func(t *testing.T) {
		report, err := parseReport([]byte(`{"status":"ok","comments":[{"path":"a"}]}`))
		if err != nil || len(report.Comments) != 1 {
			t.Fatalf("got %+v err=%v", report, err)
		}
	})
	t.Run("leading noise", func(t *testing.T) {
		report, err := parseReport([]byte("[ocr] starting\n{\"status\":\"ok\"}"))
		if err != nil || report.Status != "ok" {
			t.Fatalf("got %+v err=%v", report, err)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if _, err := parseReport(nil); err == nil {
			t.Fatal("want an error for empty output")
		}
	})
	t.Run("unparseable", func(t *testing.T) {
		err := parseReport2(t, "definitely not json")
		if !strings.Contains(err.Error(), "first 200 bytes") {
			t.Errorf("the error should show a preview, got: %v", err)
		}
	})
}

// ReviewedHead falls back when the manifest is missing, which happens on a run
// that failed before it could write one.
func TestReviewedHeadFallsBack(t *testing.T) {
	report := &ocrReport{}
	if got := report.ReviewedHead("from-webhook"); got != "from-webhook" {
		t.Errorf("got %q", got)
	}
	report.Manifest = &ocrManifest{}
	report.Manifest.Input.ResolvedHead = "  "
	if got := report.ReviewedHead("from-webhook"); got != "from-webhook" {
		t.Errorf("a blank resolved_head should fall back, got %q", got)
	}
}

func TestAskPassHelperIsWrittenAndExecutable(t *testing.T) {
	r, _ := testRunner(t)
	if err := r.ensureAskPass(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	info, err := os.Stat(r.askPass)
	if err != nil {
		t.Fatalf("the helper should exist: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("the helper must be executable, mode %v", info.Mode())
	}
	// Idempotent: a second call must not fail.
	if err := r.ensureAskPass(); err != nil {
		t.Fatalf("second call failed: %v", err)
	}
}

// The script reads the credentials out of the environment, so a prompt for the
// username must not be answered with the token.
func TestAskPassScriptTellsThePromptsApart(t *testing.T) {
	if !strings.Contains(askPassScript, "GITEE_GIT_USERNAME") ||
		!strings.Contains(askPassScript, "GITEE_GIT_TOKEN") {
		t.Fatalf("the helper must read both variables:\n%s", askPassScript)
	}
}

func TestEnsureAskPassReportsUnwritableLocation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, permissions will not block the write")
	}
	dir := t.TempDir()
	// Point the helper at a path inside a directory that cannot be written.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip("cannot make the directory read-only")
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	r, _ := testRunner(t)
	r.askPass = filepath.Join(dir, "nested", "helper.sh")
	if err := r.ensureAskPass(); err == nil {
		t.Fatal("want an error when the helper cannot be written")
	}
}

func parseReport2(t *testing.T, input string) error {
	t.Helper()
	_, err := parseReport([]byte(input))
	if err == nil {
		t.Fatal("want an error")
	}
	return err
}

// realDocument is the shape ocr actually emits, taken from a captured run and
// trimmed to the parts that matter. The one detail it exists to pin down is
// that `summary` is an object: decoding it as a string makes encoding/json
// refuse the whole document, so every real review would fail to parse and the
// service would post nothing at all. The prose lives in `project_summary`,
// which was absent in every captured run and so has to stay optional.
const realDocument = `{
  "status": "complete",
  "llm": {"provider": "anthropic", "model": "a-model"},
  "message": "review complete",
  "summary": {"files_reviewed": 39, "comments": 2, "total_tokens": 11986746,
              "input_tokens": 11455391, "output_tokens": 531355,
              "cache_read_tokens": 10928512, "elapsed": "8m18s"},
  "tool_calls": {"total": 12, "by_tool": {"read": 12}, "failure": 0},
  "comments": [
    {"path": "apps/web/app/error.tsx",
     "content": "there is no retry prop",
     "suggestion_code": "reset",
     "existing_code": "retry",
     "start_line": 5, "end_line": 7,
     "thinking": "reasoning",
     "category": "bug", "severity": "medium"}
  ],
  "groups": [{"name": "web"}],
  "session_id": "abc",
  "manifest": {
    "schema_version": "1",
    "run_id": "run-1",
    "operation": "review",
    "terminal_state": "complete",
    "repository": {"root": "/work"},
    "input": {
      "mode": "range",
      "requested_from": "base-sha",
      "requested_head": "head-sha",
      "resolved_base": "base-sha",
      "resolved_head": "23423abc50d3f8388fa9cadd7f70b0d7ff652736"
    },
    "execution": {},
    "coverage": {},
    "elapsed_ms": 498000
  }
}`

func TestParseReportReadsARealDocument(t *testing.T) {
	report, err := parseReport([]byte(realDocument))
	if err != nil {
		t.Fatalf("a document ocr really emits must parse: %v", err)
	}
	if report.Status != "complete" {
		t.Errorf("status = %q", report.Status)
	}
	if len(report.Comments) != 1 {
		t.Fatalf("comments = %d", len(report.Comments))
	}
	comment := report.Comments[0]
	if comment.Path != "apps/web/app/error.tsx" || comment.StartLine != 5 || comment.EndLine != 7 {
		t.Errorf("comment = %+v", comment)
	}
	if report.Summary == nil || report.Summary.FilesReviewed != 39 || report.Summary.Comments != 2 {
		t.Errorf("summary = %+v", report.Summary)
	}
	if got := report.ReviewedHead("fallback"); got != "23423abc50d3f8388fa9cadd7f70b0d7ff652736" {
		t.Errorf("ReviewedHead = %q", got)
	}
}

// A test for the executable wrapper itself: it is the one place a partial
// report can be lost, and cmd.Output() would lose it.
func TestExecRunnerKeepsOutputFromAFailingCommand(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "writes-then-fails.sh")
	body := "#!/bin/sh\nprintf '%s' '{\"status\":\"partial\"}'\nexit 3\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	stdout, err := execRunner{}.Run(context.Background(), dir, nil, script)
	if err == nil {
		t.Fatal("want the non-zero exit reported")
	}
	if string(stdout) != `{"status":"partial"}` {
		t.Fatalf("stdout = %q, want what the command wrote before failing", stdout)
	}
}

// The git host is derived from the API base rather than hardcoded, so the
// instance a review is fetched from and the instance its comments are posted to
// cannot disagree -- which is what the README's "change GITEE_API_BASE for Gitee
// Enterprise" promises.
func TestGitBaseFollowsTheAPIBase(t *testing.T) {
	cases := map[string]string{
		"https://gitee.com/api/v5":              "https://gitee.com",
		"https://gitee.com/api/v5/":             "https://gitee.com",
		"https://gitee.example.com/api/v5":      "https://gitee.example.com",
		"https://gitee.example.com:8443/api/v5": "https://gitee.example.com:8443",
		"  https://gitee.example.com/api/v5  ":  "https://gitee.example.com",
		"http://internal-gitee.corp/api/v5":     "http://internal-gitee.corp",
		"not a url":                             defaultCloneBase,
		"":                                      defaultCloneBase,
	}
	for apiBase, want := range cases {
		if got := gitBaseFromAPI(apiBase); got != want {
			t.Errorf("gitBaseFromAPI(%q) = %q, want %q", apiBase, got, want)
		}
	}
	if got := newRunner(&Config{GiteeAPIBase: "https://gitee.example.com/api/v5"}).cloneBase; got != "https://gitee.example.com" {
		t.Errorf("cloneBase = %q, want the host of the configured API base", got)
	}
}

func TestNewRunnerUsesTheRealCommandRunner(t *testing.T) {
	r := newRunner(&Config{ReviewTimeoutMin: 1, Concurrency: 1})
	if _, ok := r.exec.(execRunner); !ok {
		t.Fatalf("exec = %T, want the real runner so the service does not quietly do nothing", r.exec)
	}
	if !strings.HasPrefix(r.cloneBase, "https://") {
		t.Errorf("cloneBase = %q", r.cloneBase)
	}
	// The credential helper's location is a per-run decision, so a fresh runner
	// has not chosen one yet.
	if r.askPass != "" {
		t.Errorf("askPass = %q, want it unset until Prepare", r.askPass)
	}
}

// Two reviews run at once in the same process, so the credential helper has to
// be per-run: a shared path would have them writing over each other's file.
func TestPrepareGivesEachRunItsOwnCredentialHelper(t *testing.T) {
	r, _ := testRunner(t)
	job := &reviewJob{Owner: "acme", Repo: "widget", BaseSha: "a", HeadSha: "b"}

	first, err := r.Prepare(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	firstHelper := r.askPass

	second, err := r.Prepare(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	if firstHelper == "" || firstHelper == r.askPass {
		t.Fatalf("both runs used %q", firstHelper)
	}
	if _, err := os.Stat(firstHelper); err != nil {
		t.Errorf("the first run's helper is gone: %v", err)
	}
	if info, err := os.Stat(firstHelper); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the helper must not be readable by anyone else: %v", info)
	}
}

// Prepare is the seam the review and the verification round share, so the
// directory it hands back has to be real and has to be the one git ran in.
func TestPrepareReturnsAWorkspaceOverTheCheckedOutRevision(t *testing.T) {
	r, fake := testRunner(t)
	job := &reviewJob{Owner: "acme", Repo: "widget", BaseSha: "base1", HeadSha: "head1"}

	ws, err := r.Prepare(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })

	if ws.Dir() == "" {
		t.Fatal("the workspace has no directory")
	}
	info, err := os.Stat(ws.Dir())
	if err != nil || !info.IsDir() {
		t.Fatalf("the working directory was not created: %v", err)
	}
	// Every git call has to happen inside that directory, or the fetch and the
	// checkout would land on an unrelated repository the process happens to be
	// sitting in.
	call, ok := fake.find("git -c advice.detachedHead=false checkout")
	if !ok {
		t.Fatal("no checkout was run")
	}
	if call.Dir != ws.Dir() {
		t.Errorf("checkout ran in %q, workspace is %q", call.Dir, ws.Dir())
	}
}

func TestPrepareRemovesEverythingOnClose(t *testing.T) {
	r, _ := testRunner(t)
	ws, err := r.Prepare(context.Background(), &reviewJob{Owner: "acme", Repo: "widget", BaseSha: "a", HeadSha: "b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	dir := ws.Dir()
	credentials := filepath.Dir(r.askPass)
	if err := ws.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the working directory survived Close: %v", err)
	}
	if _, err := os.Stat(credentials); !os.IsNotExist(err) {
		t.Errorf("the credential directory survived Close: %v", err)
	}
	// Closing twice is not an error; shutdown paths are not careful about it.
	if err := ws.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestPrepareReportsAnUnusableWorkRoot(t *testing.T) {
	r, _ := testRunner(t)
	r.cfg.WorkRoot = filepath.Join(t.TempDir(), "does", "not", "exist")
	if _, err := r.Prepare(context.Background(), &reviewJob{Owner: "a", Repo: "b", BaseSha: "x", HeadSha: "y"}); err == nil {
		t.Fatal("want an error when the working directory cannot be created")
	}
}

// A review that fails after its directory was created has no workspace for the
// caller to close, so a failure has to leave nothing behind. Otherwise every
// failed review -- bad credentials, an unreachable commit -- leaks a checkout.
func TestPrepareRemovesWhatItCreatedWhenItFails(t *testing.T) {
	r, fake := testRunner(t)
	fake.fail["git -c advice.detachedHead=false checkout --quiet --detach head1"] = errors.New("boom")

	root := t.TempDir()
	r.cfg.WorkRoot = root
	if _, err := r.Prepare(context.Background(), &reviewJob{Owner: "a", Repo: "b", BaseSha: "x", HeadSha: "head1"}); err == nil {
		t.Fatal("want an error")
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read work root: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("a failed review left %v behind", names)
	}
}

// The verifier reads the revision's manifests through this, so it has to return
// the checked-out content and refuse anything outside the tree.
func TestWorkspaceReadFile(t *testing.T) {
	r, _ := testRunner(t)
	ws, err := r.Prepare(context.Background(), &reviewJob{Owner: "acme", Repo: "widget", BaseSha: "a", HeadSha: "b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })

	want := []byte(`{"dependencies":{"next":"16.3.0"}}`)
	if err := os.WriteFile(filepath.Join(ws.Dir(), "package.json"), want, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	got, err := ws.ReadFile("package.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("ReadFile = %q, want %q", got, want)
	}

	if _, err := ws.ReadFile("../../etc/passwd"); err == nil {
		t.Error("a path outside the working tree must not be read")
	}
	if _, err := ws.ReadFile("go.mod"); err == nil {
		t.Error("want an error for a file the revision does not have")
	}
}

// git recreates symlinks on checkout, so a merge request can name a file after
// a manifest and point it anywhere. The contents of whatever it points at would
// then be sent to the model gateway as evidence.
func TestWorkspaceReadFileRefusesASymlink(t *testing.T) {
	r, _ := testRunner(t)
	ws, err := r.Prepare(context.Background(), &reviewJob{Owner: "acme", Repo: "widget", BaseSha: "a", HeadSha: "b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })

	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("token=abcdef"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.Symlink(secret, filepath.Join(ws.Dir(), "package.json")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := ws.ReadFile("package.json"); err == nil {
		t.Fatal("a symlink must not be followed out of the working tree")
	}
}

// A directory named after a manifest is not a manifest either.
func TestWorkspaceReadFileRefusesADirectory(t *testing.T) {
	r, _ := testRunner(t)
	ws, err := r.Prepare(context.Background(), &reviewJob{Owner: "acme", Repo: "widget", BaseSha: "a", HeadSha: "b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Cleanup(func() { _ = ws.Close() })

	if err := os.Mkdir(filepath.Join(ws.Dir(), "package.json"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := ws.ReadFile("package.json"); err == nil {
		t.Fatal("want an error for a directory")
	}
}
