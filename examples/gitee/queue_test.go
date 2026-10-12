// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func job(number int, head string) *reviewJob {
	return &reviewJob{Owner: "acme", Repo: "widget", Number: number, HeadSha: head, BaseSha: "base"}
}

// waitFor polls a condition instead of sleeping a fixed amount, so the test
// neither flakes on a slow machine nor wastes time on a fast one.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestQueueRejectsDuplicateDeliveries(t *testing.T) {
	q := newQueue(1)
	release := make(chan struct{})
	started := make(chan struct{}, 1)

	run := func(ctx context.Context, j *reviewJob) error {
		started <- struct{}{}
		<-release
		return nil
	}

	if !q.Enqueue(context.Background(), job(1, "sha1"), run) {
		t.Fatal("the first delivery should be accepted")
	}
	<-started

	// The redelivery arrives while the first run is still going, which is the
	// case that matters: Gitee sends more than one event per push.
	if q.Enqueue(context.Background(), job(1, "sha1"), run) {
		t.Fatal("a redelivery of the same head sha must be rejected")
	}
	close(release)
	q.Wait()

	// And again after it finished, since a late redelivery is still redundant.
	if q.Enqueue(context.Background(), job(1, "sha1"), run) {
		t.Fatal("an already-reviewed sha must stay rejected")
	}
}

func TestQueueAcceptsNewHeadShaForTheSameRequest(t *testing.T) {
	q := newQueue(1)
	var runs int32
	run := func(ctx context.Context, j *reviewJob) error {
		atomic.AddInt32(&runs, 1)
		return nil
	}
	if !q.Enqueue(context.Background(), job(1, "sha1"), run) {
		t.Fatal("first job rejected")
	}
	if !q.Enqueue(context.Background(), job(1, "sha2"), run) {
		t.Fatal("a new head sha is a new review and must be accepted")
	}
	q.Wait()
	if got := atomic.LoadInt32(&runs); got != 2 {
		t.Fatalf("want 2 runs, got %d", got)
	}
}

func TestQueueDeduplicatesPerRequestNotGlobally(t *testing.T) {
	q := newQueue(2)
	var mu sync.Mutex
	seen := map[string]int{}
	run := func(ctx context.Context, j *reviewJob) error {
		mu.Lock()
		seen[j.String()]++
		mu.Unlock()
		return nil
	}
	// Same sha, different merge requests: unrelated work, both must run.
	q.Enqueue(context.Background(), job(1, "sha1"), run)
	q.Enqueue(context.Background(), job(2, "sha1"), run)
	q.Wait()
	mu.Lock()
	defer mu.Unlock()
	if seen["acme/widget#1"] != 1 || seen["acme/widget#2"] != 1 {
		t.Fatalf("both requests should have run once, got %v", seen)
	}
}

// A review costs an LLM call per file group, so the cap has to actually cap.
func TestQueueRespectsTheConcurrencyLimit(t *testing.T) {
	const limit = 2
	q := newQueue(limit)

	var mu sync.Mutex
	running, peak := 0, 0
	release := make(chan struct{})

	run := func(ctx context.Context, j *reviewJob) error {
		mu.Lock()
		running++
		if running > peak {
			peak = running
		}
		mu.Unlock()
		<-release
		mu.Lock()
		running--
		mu.Unlock()
		return nil
	}

	for i := 1; i <= 6; i++ {
		if !q.Enqueue(context.Background(), job(i, "sha"), run) {
			t.Fatalf("job %d rejected", i)
		}
	}
	waitFor(t, "the limit to be reached", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return running == limit
	})
	mu.Lock()
	got := peak
	mu.Unlock()
	if got > limit {
		t.Fatalf("peak concurrency %d exceeded the limit %d", got, limit)
	}
	close(release)
	q.Wait()
}

// Two runs for one merge request would race to post, and the slower one would
// order its comments against code that has already moved on.
func TestQueueSerializesPerMergeRequest(t *testing.T) {
	q := newQueue(4)

	var mu sync.Mutex
	running := map[int]int{}
	overlap := false
	release := make(chan struct{})

	run := func(ctx context.Context, j *reviewJob) error {
		mu.Lock()
		running[j.Number]++
		if running[j.Number] > 1 {
			overlap = true
		}
		mu.Unlock()
		<-release
		mu.Lock()
		running[j.Number]--
		mu.Unlock()
		return nil
	}

	// Different head shas for the same request, so dedup lets both through and
	// only the per-request lock can keep them apart.
	q.Enqueue(context.Background(), job(7, "sha1"), run)
	q.Enqueue(context.Background(), job(7, "sha2"), run)
	waitFor(t, "the first run to start", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return running[7] == 1
	})
	// Give the second run a chance to violate the lock before releasing.
	time.Sleep(20 * time.Millisecond)
	close(release)
	q.Wait()

	mu.Lock()
	defer mu.Unlock()
	if overlap {
		t.Fatal("two reviews for the same merge request ran at the same time")
	}
}

func TestQueueCloseRefusesNewWork(t *testing.T) {
	q := newQueue(1)
	run := func(ctx context.Context, j *reviewJob) error { return nil }
	q.Close()
	if q.Enqueue(context.Background(), job(1, "sha"), run) {
		t.Fatal("a closed queue must refuse work")
	}
}

// A panic inside one review must not take the process down: the remaining
// deliveries still deserve a review.
func TestQueueSurvivesAPanickingReview(t *testing.T) {
	q := newQueue(1)
	var ran int32
	q.Enqueue(context.Background(), job(1, "sha1"), func(ctx context.Context, j *reviewJob) error {
		panic("boom")
	})
	q.Enqueue(context.Background(), job(2, "sha2"), func(ctx context.Context, j *reviewJob) error {
		atomic.AddInt32(&ran, 1)
		return nil
	})
	q.Wait()
	if got := atomic.LoadInt32(&ran); got != 1 {
		t.Fatalf("the review after the panic should still have run, ran=%d", got)
	}
}

func TestJobKeyIncludesTheHeadSha(t *testing.T) {
	a := jobKey(job(1, "sha1"))
	b := jobKey(job(1, "sha2"))
	c := jobKey(job(2, "sha1"))
	if a == b {
		t.Error("a different head sha must produce a different key")
	}
	if a == c {
		t.Error("a different merge request must produce a different key")
	}
}

// A service runs for months, so remembering every revision it has ever seen
// would grow without bound. The oldest are forgotten once the window is full,
// which costs a repeated review rather than memory.
func TestQueueForgetsTheOldestRevisionsOnceTheWindowIsFull(t *testing.T) {
	q := newQueue(4)
	run := func(context.Context, *reviewJob) error { return nil }

	for index := range maxRemembered + 1 {
		if !q.Enqueue(context.Background(), job(1, fmt.Sprintf("sha%d", index)), run) {
			t.Fatalf("job %d was refused", index)
		}
	}
	q.Wait()

	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.accepted) > maxRemembered || len(q.order) > maxRemembered {
		t.Fatalf("the window grew to %d entries", len(q.accepted))
	}
	if q.accepted[jobKey(job(1, "sha0"))] {
		t.Error("the oldest revision should have been forgotten")
	}
	if !q.accepted[jobKey(job(1, fmt.Sprintf("sha%d", maxRemembered)))] {
		t.Error("the newest revision should still be remembered")
	}
}

// The per-request lock is what serialises a push against the review already
// running for that request. It has to outlive the job, and it must not outlive
// the last job that uses it.
func TestQueueReleasesThePerRequestLockWithItsLastJob(t *testing.T) {
	q := newQueue(4)
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	run := func(context.Context, *reviewJob) error {
		started <- struct{}{}
		<-release
		return nil
	}

	if !q.Enqueue(context.Background(), job(1, "sha1"), run) {
		t.Fatal("the first job was refused")
	}
	<-started

	// A second push to the same request is accepted, and waits on the lock.
	if !q.Enqueue(context.Background(), job(1, "sha2"), run) {
		t.Fatal("the second job was refused")
	}
	q.mu.Lock()
	lock, ok := q.prLocks[jobKeyPR(job(1, "sha1"))]
	if !ok {
		q.mu.Unlock()
		t.Fatal("the two pushes should share one lock")
	}
	if lock.refs != 2 {
		q.mu.Unlock()
		t.Fatalf("refs = %d, want both jobs counted", lock.refs)
	}
	q.mu.Unlock()

	close(release)
	q.Wait()

	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.prLocks) != 0 {
		t.Errorf("the lock outlived its jobs: %d left", len(q.prLocks))
	}
}
