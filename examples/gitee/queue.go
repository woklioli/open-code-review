// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"fmt"
	"log"
	"sync"
)

// reviewFunc is the work a queued job maps to.
type reviewFunc func(ctx context.Context, job *reviewJob) error

// queue bounds how much review work runs at once and keeps it from being
// started twice.
//
// Two properties matter more than throughput here. A review costs an LLM call
// per file group, so an unbounded queue turns a burst of pushes into an
// unbounded bill. And a Gitee merge request produces several deliveries for
// what a human considers one event -- opened, then synchronize for each push --
// so without deduplication the same commit is reviewed repeatedly and the
// request collects near-identical comments.
// maxRemembered bounds the deduplication memory. Gitee redelivers a webhook
// within minutes, so a window of the most recent revisions covers every retry
// that matters; past it the oldest key is forgotten, which costs a repeated
// review rather than unbounded growth in a process that runs for months. A
// forgotten key is not harmful: the poster's own markers still keep the
// comments from being posted twice.
const maxRemembered = 4096

type queue struct {
	slots chan struct{}

	mu       sync.Mutex
	accepted map[string]bool
	order    []string
	inFlight int
	prLocks  map[string]*prLock

	wg     sync.WaitGroup
	closed bool
}

// prLock serialises the reviews of one merge request, and counts the jobs that
// reference it so it can be removed once the last one finishes.
type prLock struct {
	mu   sync.Mutex
	refs int
}

func newQueue(concurrency int) *queue {
	if concurrency < 1 {
		concurrency = 1
	}
	return &queue{
		slots:    make(chan struct{}, concurrency),
		accepted: map[string]bool{},
		prLocks:  map[string]*prLock{},
	}
}

// jobKey identifies one review run. The head sha is part of it on purpose: a
// push that changes the branch is a genuinely new review, while a redelivery
// of the same push is not.
func jobKey(job *reviewJob) string {
	return fmt.Sprintf("%s/%s#%d@%s", job.Owner, job.Repo, job.Number, job.HeadSha)
}

// Enqueue accepts a job for background review, reporting whether it was taken.
// A false return is not an error: it means this delivery duplicates work that
// is already queued, running, or finished.
func (q *queue) Enqueue(ctx context.Context, job *reviewJob, run reviewFunc) bool {
	key := jobKey(job)

	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return false
	}
	if q.accepted[key] {
		q.mu.Unlock()
		return false
	}
	// Marked accepted before the goroutine starts, not when it finishes. The
	// duplicate deliveries that matter arrive while the first is still running.
	q.accepted[key] = true
	q.order = append(q.order, key)
	q.inFlight++
	// The eviction happens before the job runs rather than after, so the bound
	// holds however long a review takes. A key that is still running can be
	// evicted, which is why in-flight work is counted separately rather than
	// derived from the size of this map.
	for len(q.order) > maxRemembered {
		delete(q.accepted, q.order[0])
		q.order = q.order[1:]
	}
	prLock := q.prLockFor(job)
	// Counted while the lock is still held, so there is no instant in which this
	// job is registered but invisible to the WaitGroup. A Close and Wait landing
	// in that window would see a zero counter, return, and let shutdown cancel a
	// review that is still running.
	q.wg.Add(1)
	q.mu.Unlock()

	go func() {
		defer q.wg.Done()
		defer func() {
			q.mu.Lock()
			q.inFlight--
			// The lock is dropped with its last job, so a service that has seen
			// many merge requests does not hold a mutex for each of them.
			if prLock.refs--; prLock.refs == 0 {
				delete(q.prLocks, jobKeyPR(job))
			}
			q.mu.Unlock()
		}()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("review %s panicked: %v", job, r)
			}
		}()

		// One review at a time per merge request even when the head sha
		// differs. Two runs for the same request would race to post, and the
		// slower one would order its comments against code that has already
		// moved on.
		prLock.mu.Lock()
		defer prLock.mu.Unlock()

		q.slots <- struct{}{}
		defer func() { <-q.slots }()

		if err := run(ctx, job); err != nil {
			log.Printf("review %s failed: %v", job, err)
		}
	}()
	return true
}

// jobKeyPR names a merge request, without the revision: two pushes to the same
// request are different jobs but share one lock.
func jobKeyPR(job *reviewJob) string {
	return fmt.Sprintf("%s/%s#%d", job.Owner, job.Repo, job.Number)
}

// prLockFor returns the merge request's lock, creating it and taking a
// reference on it. The caller holds q.mu.
func (q *queue) prLockFor(job *reviewJob) *prLock {
	key := jobKeyPR(job)
	lock, ok := q.prLocks[key]
	if !ok {
		lock = &prLock{}
		q.prLocks[key] = lock
	}
	lock.refs++
	return lock
}

// Wait blocks until every accepted job has finished. Shutdown calls it after
// Close so a container stop does not abandon a review midway and leave a
// half-posted set of comments behind.
func (q *queue) Wait() { q.wg.Wait() }

// Close refuses further work. Jobs already accepted still run to completion.
func (q *queue) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
}

// InFlight reports how many accepted jobs have not finished. It exists for the
// health endpoint, where a count that never drops is the signal that a review
// is wedged.
func (q *queue) InFlight() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.inFlight
}
