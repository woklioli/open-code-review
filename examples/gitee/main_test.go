// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testService wires a service whose reviews finish immediately, so the HTTP
// tests exercise routing rather than git and an ocr binary.
func testService(t *testing.T) (*service, *recordedRuns) {
	t.Helper()
	cfg := &Config{
		WebhookSecret:    "s3cret",
		GiteeToken:       "token",
		LLMURL:           "https://gateway.example",
		LLMToken:         "llm-token",
		LLMModel:         "a-model",
		Concurrency:      2,
		ReviewTimeoutMin: 1,
		DryRun:           true,
	}
	svc := newService(cfg)
	runs := &recordedRuns{}
	svc.run = runs.fn
	t.Cleanup(func() { svc.shutdown(time.Second) })
	return svc, runs
}

type recordedRuns struct {
	mu   sync.Mutex
	jobs []reviewJob
	done chan struct{}
}

func (r *recordedRuns) fn(_ context.Context, job *reviewJob) error {
	r.mu.Lock()
	r.jobs = append(r.jobs, *job)
	done := r.done
	r.mu.Unlock()
	if done != nil {
		select {
		case done <- struct{}{}:
		default:
		}
	}
	return nil
}

func (r *recordedRuns) recorded() []reviewJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]reviewJob(nil), r.jobs...)
}

// waitForRun blocks until the queue has started a review, so a test asserts on
// what happened rather than on how fast the scheduler is.
func (r *recordedRuns) waitForRun(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(r.recorded()) > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("no review was started")
}

func signedRequest(t *testing.T, secret string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	req.Header.Set("X-Gitee-Signature", signBody(secret, body))
	return req
}

func postWebhook(t *testing.T, svc *service, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	svc.handleWebhook(recorder, signedRequest(t, svc.cfg.WebhookSecret, body))
	return recorder
}

func TestWebhookQueuesAReview(t *testing.T) {
	svc, runs := testService(t)
	runs.done = make(chan struct{}, 1)

	recorder := postWebhook(t, svc, payload(nil))

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body %q", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "queued") {
		t.Errorf("body = %q", recorder.Body.String())
	}
	runs.waitForRun(t)
	got := runs.recorded()
	if len(got) != 1 || got[0].Owner != "acme" || got[0].Number != 42 || got[0].HeadSha != "aaaa1111" {
		t.Fatalf("jobs = %+v", got)
	}
}

// Gitee delivers one push as several events. The second must be a quiet no-op:
// the same commit reviewed twice would duplicate every comment on the request.
func TestWebhookAcceptsADuplicateWithoutReviewingItTwice(t *testing.T) {
	svc, runs := testService(t)
	runs.done = make(chan struct{}, 1)

	if code := postWebhook(t, svc, payload(nil)).Code; code != http.StatusAccepted {
		t.Fatalf("first delivery: %d", code)
	}
	runs.waitForRun(t)

	recorder := postWebhook(t, svc, payload(nil))
	if recorder.Code != http.StatusAccepted {
		t.Errorf("a duplicate is not an error, got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "already queued") {
		t.Errorf("body = %q, want it to say the work was already taken", recorder.Body.String())
	}
	if len(runs.recorded()) != 1 {
		t.Errorf("ran %d times", len(runs.recorded()))
	}
}

func TestWebhookRejectsBadRequests(t *testing.T) {
	cases := map[string]struct {
		method string
		body   []byte
		sign   bool
		want   int
	}{
		"wrong method":    {method: http.MethodGet, body: payload(nil), sign: true, want: http.StatusMethodNotAllowed},
		"unsigned":        {method: http.MethodPost, body: payload(nil), sign: false, want: http.StatusForbidden},
		"unrecognised":    {method: http.MethodPost, body: []byte(`{"action":"teleported"}`), sign: true, want: http.StatusBadRequest},
		"not json at all": {method: http.MethodPost, body: []byte(`}{`), sign: true, want: http.StatusBadRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			svc, runs := testService(t)
			req := httptest.NewRequest(tc.method, "/webhook", strings.NewReader(string(tc.body)))
			if tc.sign {
				req.Header.Set("X-Gitee-Signature", signBody(svc.cfg.WebhookSecret, tc.body))
			}
			recorder := httptest.NewRecorder()
			svc.handleWebhook(recorder, req)
			if recorder.Code != tc.want {
				t.Errorf("status = %d, want %d", recorder.Code, tc.want)
			}
			if len(runs.recorded()) != 0 {
				t.Errorf("nothing should have been queued: %+v", runs.recorded())
			}
		})
	}
}

// An event the service recognises but does not act on -- a draft, a close --
// is accepted and dropped. Returning an error would make Gitee retry it.
func TestWebhookIgnoresEventsWithNothingToReview(t *testing.T) {
	svc, runs := testService(t)

	body := payload(map[string]any{"action": "close"})
	recorder := postWebhook(t, svc, body)

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "ignored") {
		t.Errorf("body = %q", recorder.Body.String())
	}
	if len(runs.recorded()) != 0 {
		t.Errorf("queued %+v", runs.recorded())
	}
}

func TestWebhookRejectsABodyThatIsTooLarge(t *testing.T) {
	svc, _ := testService(t)
	// A webhook payload is a few kilobytes. Accepting an unbounded one would let
	// anyone who can reach the port make the service allocate.
	body := make([]byte, maxPayloadBytes+1)
	for index := range body {
		body[index] = 'a'
	}
	recorder := postWebhook(t, svc, body)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", recorder.Code)
	}
}

func TestHealthEndpointReportsInFlightWork(t *testing.T) {
	svc, _ := testService(t)
	recorder := httptest.NewRecorder()
	svc.handleHealth(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if got := recorder.Body.String(); !strings.Contains(got, "in_flight=0") {
		t.Errorf("body = %q", got)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
		t.Errorf("Content-Type = %q", contentType)
	}
}

func TestHandlerRoutesOnlyTheTwoEndpoints(t *testing.T) {
	svc, _ := testService(t)
	handler := svc.handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recorder.Code != http.StatusOK {
		t.Errorf("/healthz = %d", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusNotFound {
		t.Errorf("/ = %d, want 404", recorder.Code)
	}
}

// A response has to be written before the review starts, or Gitee times the
// webhook out and redelivers it while the first review is still running.
func TestWebhookRespondsBeforeTheReviewFinishes(t *testing.T) {
	svc, _ := testService(t)
	release := make(chan struct{})
	started := make(chan struct{})
	svc.run = func(context.Context, *reviewJob) error {
		close(started)
		<-release
		return nil
	}
	t.Cleanup(func() { close(release) })

	recorder := httptest.NewRecorder()
	svc.handleWebhook(recorder, signedRequest(t, svc.cfg.WebhookSecret, payload(nil)))

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d", recorder.Code)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the review never started")
	}
}

// The verification round is an addition, and switching it off must leave the
// review exactly as ocr produced it.
func TestVerifyIsSkippedWhenDisabled(t *testing.T) {
	cfg := &Config{VerifyDeps: false}
	svc := newService(cfg)
	report := &ocrReport{Comments: []ocrComment{{Content: retryPropFinding}}}

	options := svc.verify(context.Background(), &workspace{}, report)

	if options.Dropped != 0 || len(options.Notes) != 0 {
		t.Errorf("options = %+v", options)
	}
	if len(report.Comments) != 1 {
		t.Error("the finding should have been left alone")
	}
}

// Enabled but with nothing to check: the workspace is never touched, so a
// review that found nothing does not depend on a checkout being readable.
func TestVerifyDoesNothingWithoutFindings(t *testing.T) {
	svc := newService(&Config{VerifyDeps: true})
	report := &ocrReport{}
	if options := svc.verify(context.Background(), &workspace{}, report); options.Dropped != 0 {
		t.Errorf("options = %+v", options)
	}
}

// Enabled with findings, over a revision whose manifests pin nothing this can
// check. The round has to reach that conclusion and stop, and it has to leave
// the report exactly as it found it.
func TestVerifyKeepsFindingsItCannotCheck(t *testing.T) {
	svc := newService(&Config{
		VerifyDeps: true,
		LLMURL:     "https://gateway.example",
		LLMToken:   "llm-token",
		LLMModel:   "a-model",
	})
	report := &ocrReport{Comments: []ocrComment{{Path: "app/error.tsx", Content: retryPropFinding}}}

	options := svc.verify(context.Background(), &workspace{dir: t.TempDir()}, report)

	if options.Dropped != 0 {
		t.Errorf("nothing could be pinned, so nothing could be dropped: %+v", options)
	}
	if len(report.Comments) != 1 {
		t.Fatalf("the finding was removed: %+v", report.Comments)
	}
}

// A review that cannot get the commits is an error the queue logs; it must not
// reach the poster with a report it does not have.
func TestReviewReportsAWorkspaceItCannotPrepare(t *testing.T) {
	svc := newService(&Config{
		WorkRoot:         filepath.Join(t.TempDir(), "does", "not", "exist"),
		ReviewTimeoutMin: 1,
		Concurrency:      1,
	})
	err := svc.review(context.Background(), &reviewJob{Owner: "acme", Repo: "widget", Number: 1, BaseSha: "a", HeadSha: "b"})
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "acme/widget#1") {
		t.Errorf("the error should name the merge request, got %v", err)
	}
}

func TestShutdownDrainsQueuedWork(t *testing.T) {
	svc, runs := testService(t)
	finished := make(chan struct{})
	svc.run = func(context.Context, *reviewJob) error {
		defer close(finished)
		runs.fn(context.Background(), &reviewJob{})
		return nil
	}

	if !svc.queue.Enqueue(svc.ctx, &reviewJob{Owner: "acme", Repo: "widget", Number: 1, HeadSha: "a"}, svc.run) {
		t.Fatal("the job was not accepted")
	}
	svc.shutdown(5 * time.Second)

	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("shutdown returned before the review finished")
	}
	// Further webhooks are refused once the queue is closed.
	if svc.queue.Enqueue(svc.ctx, &reviewJob{Owner: "acme", Repo: "widget", Number: 2, HeadSha: "b"}, svc.run) {
		t.Error("a closed queue should refuse new work")
	}
}

func TestNewServiceStartsUsable(t *testing.T) {
	svc := newService(&Config{Concurrency: 3})
	if svc.run == nil || svc.queue == nil || svc.ctx == nil {
		t.Fatalf("service = %+v", svc)
	}
	if svc.cfg.Concurrency != 3 {
		t.Errorf("concurrency = %d", svc.cfg.Concurrency)
	}
	svc.cancel()
}

func TestReviewBudgetLeavesRoomForOcrToGiveUpFirst(t *testing.T) {
	// ocr has to reach its own --timeout and still write a report; being killed
	// from outside loses every finding the run had produced.
	if reviewSlack <= 0 {
		t.Errorf("reviewSlack = %v", reviewSlack)
	}
	if shutdownGrace <= 0 {
		t.Errorf("shutdownGrace = %v", shutdownGrace)
	}
}

func TestRespondCarriesTheMessage(t *testing.T) {
	recorder := httptest.NewRecorder()
	respond(recorder, http.StatusTeapot, "hello")
	if recorder.Code != http.StatusTeapot {
		t.Errorf("status = %d", recorder.Code)
	}
	if got := recorder.Body.String(); got != "hello\n" {
		t.Errorf("body = %q", got)
	}
}

// The job's identity has to survive a round trip through the queue: a
// mislabelled run would post one merge request's findings onto another.
func TestQueuedJobKeepsItsIdentity(t *testing.T) {
	svc, runs := testService(t)
	runs.done = make(chan struct{}, 1)

	body := payload(map[string]any{"number": 7, "pull_request": map[string]any{
		"number": 7,
		"title":  "Fix the parser",
		"state":  "open",
		"head":   map[string]any{"ref": "fix", "sha": "cafe1234"},
		"base":   map[string]any{"ref": "main", "sha": "beef5678"},
	}})
	if code := postWebhook(t, svc, body).Code; code != http.StatusAccepted {
		t.Fatalf("status = %d", code)
	}
	runs.waitForRun(t)

	got := runs.recorded()[0]
	if got.Number != 7 || got.HeadSha != "cafe1234" || got.BaseSha != "beef5678" || got.Title != "Fix the parser" {
		encoded, _ := json.Marshal(got)
		t.Errorf("job = %s", encoded)
	}
}
