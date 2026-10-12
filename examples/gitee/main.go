// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// reviewSlack is added to the review budget on top of ocr's own --timeout.
// Letting ocr reach its own timeout first is deliberate: it then writes the
// findings it did produce, and a partial report still posts. Being killed from
// outside instead loses everything the run had found.
const reviewSlack = 5 * time.Minute

// shutdownGrace bounds how long a container stop waits for in-flight reviews.
const shutdownGrace = 2 * time.Minute

func main() {
	envFile := flag.String("env", "", "path to a dotenv file; real environment variables win over it")
	flag.Parse()

	// UTC everywhere: the service runs in a container whose local time is
	// whatever the host decided, and correlating logs with Gitee's own
	// timestamps is the only thing anyone does with them.
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := LoadConfig(*envFile)
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}
	log.Printf("open-code-review gitee service | %s", cfg.Redacted())
	if cfg.DryRun {
		log.Print("DRY RUN is on: nothing will be posted. Set GITEE_DRY_RUN=false to post for real.")
	}
	if cfg.VerifyDeps {
		log.Print("dependency verification is on: findings about a pinned dependency may be dropped when the package contradicts them")
	}

	svc := newService(cfg)
	server := &http.Server{
		Addr:    cfg.Addr,
		Handler: svc.handler(),
		// A webhook body arrives in one small request; a slow or stalled client
		// must not be able to hold a connection open indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("listening on %s", cfg.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Print("shutting down: refusing new webhooks, waiting for in-flight reviews")

	// The order matters. ListenAndServe stops first so no new job is accepted
	// while the queue drains, and the queue then drains before the process
	// exits -- a review abandoned midway would leave a merge request with some
	// of its comments posted and no summary saying so.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
	svc.shutdown(shutdownGrace)
	log.Print("stopped")
}

type service struct {
	cfg   *Config
	queue *queue
	// run is the review function the queue executes. It is a field so the HTTP
	// tests can replace it and exercise routing without git or an ocr binary.
	run reviewFunc

	// ctx parents every review. It is deliberately not the webhook request's
	// context: the response is sent as soon as the job is queued, and a review
	// inheriting the request would be cancelled the moment that response is
	// written, which is always before the review has done anything.
	ctx    context.Context
	cancel context.CancelFunc

	logf func(format string, args ...any)
}

func newService(cfg *Config) *service {
	base, cancel := context.WithCancel(context.Background())
	svc := &service{
		cfg:    cfg,
		queue:  newQueue(cfg.Concurrency),
		ctx:    base,
		cancel: cancel,
		logf:   log.Printf,
	}
	svc.run = svc.review
	return svc
}

func (s *service) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", s.handleWebhook)
	mux.HandleFunc("/healthz", s.handleHealth)
	return mux
}

func (s *service) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		s.logf("webhook: cannot read the body: %v", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := VerifyRequest(s.cfg.WebhookSecret, r.Header, body); err != nil {
		// The reason goes to the log and not to the caller: a prober should not
		// learn which header or which scheme would have worked.
		s.logf("webhook rejected: %v", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	job, err := ParseEvent(body)
	if err != nil {
		s.logf("webhook payload: %v", err)
		http.Error(w, "unrecognised payload", http.StatusBadRequest)
		return
	}
	if job == nil {
		// Recognised, but there is nothing to review: a draft, or an action
		// this service does not act on.
		respond(w, http.StatusAccepted, "ignored")
		return
	}
	if !s.queue.Enqueue(s.ctx, job, s.run) {
		// Not an error. Gitee sends several deliveries for one push, and the
		// extra ones are expected to land here.
		respond(w, http.StatusAccepted, "already queued")
		return
	}
	s.logf("queued %s at %s", job, shortSHA(job.HeadSha))
	respond(w, http.StatusAccepted, "queued")
}

func (s *service) handleHealth(w http.ResponseWriter, r *http.Request) {
	// In-flight is the number that matters operationally: a count that never
	// falls means a review is wedged, and Gitee will keep retrying the webhook.
	respond(w, http.StatusOK, fmt.Sprintf("ok in_flight=%d", s.queue.InFlight()))
}

// review is the whole job: fetch the range, review it, verify what came back,
// and post it.
func (s *service) review(ctx context.Context, job *reviewJob) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.cfg.ReviewTimeoutMin)*time.Minute+reviewSlack)
	defer cancel()

	// The checkout outlives the review: the verification round reads the
	// revision's manifests out of it to find which dependency versions were
	// actually in play.
	workspace, err := newRunner(s.cfg).Prepare(ctx, job)
	if err != nil {
		return fmt.Errorf("%s: %w", job, err)
	}
	defer workspace.Close()

	report, err := workspace.Run(ctx)
	if err != nil {
		return fmt.Errorf("%s: %w", job, err)
	}

	// The commit ocr resolved, not the one the webhook named: the branch may
	// have moved while the review was running, and a diff position is only
	// meaningful against the diff it was computed from.
	commitID := report.ReviewedHead(job.HeadSha)

	options := s.verify(ctx, workspace, report)

	var api posterAPI = newGiteeClient(s.cfg)
	if s.cfg.DryRun {
		api = &dryRunAPI{inner: api, logf: s.logf}
	}
	result, err := newPoster(api, s.logf).Post(ctx, job, commitID, report, options)
	if err != nil {
		return fmt.Errorf("%s: %w", job, err)
	}
	s.logf("%s on %s: %d annotated, %d listed, %d already reported, %d removed",
		job, shortSHA(commitID), result.Inline, len(result.Folded), result.Skipped, options.Dropped)
	return nil
}

// verify runs the dependency verification round when it is switched on. A
// failure here never fails the job: findings are kept, which is what would have
// happened had the round not run.
func (s *service) verify(ctx context.Context, workspace *workspace, report *ocrReport) postOptions {
	if !s.cfg.VerifyDeps || len(report.Comments) == 0 {
		return postOptions{}
	}
	verifier := &dependencyVerifier{
		source:   workspace,
		evidence: newNPMCDN(newEvidenceHTTPClient(), s.logf),
		judge:    newJudgeClient(s.cfg, newEvidenceHTTPClient(), s.logf),
		logf:     s.logf,
	}
	return verifier.Verify(ctx, report)
}

// shutdown drains the queue, giving up after grace.
func (s *service) shutdown(grace time.Duration) {
	s.queue.Close()
	done := make(chan struct{})
	go func() {
		s.queue.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
		// Cancelling is the only lever left; a review that ignores its context
		// is not going to finish, and the process needs to exit.
		s.logf("in-flight reviews did not finish within %s; cancelling them", grace)
	}
	s.cancel()
}

func respond(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, message+"\n")
}
