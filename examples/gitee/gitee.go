// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxResponseBytes caps one API response. A merge request's file list carries a
// patch per file, so the largest legitimate response is a big merge request;
// the cap exists so a misbehaving or hostile endpoint cannot make the service
// allocate without bound.
const maxResponseBytes = 8 << 20

// patchField decodes the `patch` member of a PR file.
//
// Gitee returns it as an object ({"diff": "..."}) on the v5 API this service
// talks to, while GitHub and every Gitee example return a bare string. Both
// spellings are accepted because the cost is a few lines and the failure mode
// of guessing wrong is every inline comment silently losing its position.
type patchField struct {
	Diff string
}

func (p *patchField) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		return json.Unmarshal(data, &p.Diff)
	}
	var wrapper struct {
		Diff string `json:"diff"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return fmt.Errorf("patch is neither a string nor an object with a diff field: %w", err)
	}
	p.Diff = wrapper.Diff
	return nil
}

// pullRequestFile is one changed file in a merge request.
type pullRequestFile struct {
	Filename string     `json:"filename"`
	Sha      string     `json:"sha"`
	Status   string     `json:"status"`
	Patch    patchField `json:"patch"`
}

// pullRequestComment is one review comment. Line comments carry a path; the
// summary comments this service writes and updates are the ones without one.
type pullRequestComment struct {
	ID          int64  `json:"id"`
	Path        string `json:"path"`
	Position    *int   `json:"position"`
	NewLine     *int   `json:"new_line"`
	CommitID    string `json:"commit_id"`
	Body        string `json:"body"`
	CommentType string `json:"comment_type"`
	CreatedAt   string `json:"created_at"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
}

// giteeClient is a thin REST client. It holds no state beyond the connection,
// so one instance serves every repository the service is configured for.
type giteeClient struct {
	base   string
	token  string
	client *http.Client

	// maxRetries bounds how many times a retryable response is attempted. The
	// default is small on purpose: a review that cannot post is better reported
	// than retried until the container is killed.
	maxRetries int
	// backoff is the first retry delay; each further attempt doubles it. Kept
	// as a field so tests do not sleep through the schedule.
	backoff time.Duration
}

func newGiteeClient(cfg *Config) *giteeClient {
	return &giteeClient{
		base:       strings.TrimRight(cfg.GiteeAPIBase, "/"),
		token:      cfg.GiteeToken,
		client:     &http.Client{Timeout: 60 * time.Second},
		maxRetries: 3,
		backoff:    2 * time.Second,
	}
}

// giteeError carries the status so a caller can tell a permanent rejection
// from a transient one.
type giteeError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *giteeError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 300 {
		body = body[:300] + "..."
	}
	return fmt.Sprintf("gitee %s %s: status %d: %s", e.Method, e.Path, e.Status, body)
}

func (e *giteeError) retryable() bool {
	return e.Status == http.StatusTooManyRequests ||
		e.Status == http.StatusRequestTimeout ||
		e.Status >= 500
}

// isIdempotent reports whether resending a request is safe. Creating a comment
// is not: if the response is lost after Gitee wrote the comment, sending it
// again posts a second copy, and nothing downstream can tell the two apart.
func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// do performs a request, retrying the responses worth retrying.
//
// The token goes in the Authorization header rather than an access_token query
// parameter: query strings land in access logs and proxy traces, and this token
// can write to every repository the operator can.
func (c *giteeClient) do(ctx context.Context, method, path string, form url.Values, out any) error {
	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := c.backoff << (attempt - 1)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		endpoint := c.base + path
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Authorization", "token "+c.token)
		req.Header.Set("Accept", "application/json")
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}

		resp, err := c.client.Do(req)
		if err != nil {
			// A transport error on a write is ambiguous -- the request may have
			// been carried out before the connection broke -- so it is reported
			// rather than retried. The caller folds the finding into the summary,
			// which is better than posting it twice.
			lastErr = fmt.Errorf("%s %s: %w", method, path, err)
			if !isIdempotent(method) {
				return lastErr
			}
			continue
		}
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("%s %s: read body: %w", method, path, readErr)
			if !isIdempotent(method) {
				return lastErr
			}
			continue
		}
		if int64(len(payload)) > maxResponseBytes {
			// Reported rather than handed to the JSON decoder, where a truncated
			// body fails as "unexpected end of JSON input" and hides the real
			// cause. Not retried: the same request would fetch the same page.
			return fmt.Errorf("%s %s: response exceeded the %d byte limit", method, path, maxResponseBytes)
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if out != nil && len(bytes.TrimSpace(payload)) > 0 {
				if err := json.Unmarshal(payload, out); err != nil {
					return fmt.Errorf("%s %s: decode response: %w", method, path, err)
				}
			}
			return nil
		}

		apiErr := &giteeError{Status: resp.StatusCode, Method: method, Path: path, Body: string(payload)}
		if !apiErr.retryable() {
			return apiErr
		}
		// A rate limit or a timeout is a refusal, so the comment was not
		// created and resending it cannot duplicate anything. A 5xx is not: the
		// server may have written the comment before it failed.
		if !isIdempotent(method) && resp.StatusCode >= 500 {
			return apiErr
		}
		lastErr = apiErr
	}
	return lastErr
}

func repoPath(owner, repo string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

// pageSize is what the API accepts per request. Every list method follows
// pages until a short one arrives: a truncated list of changed files loses the
// positions for the files it left out, and a truncated list of comments loses
// the sticky summary and the markers of what was already posted, which is what
// makes a second run duplicate every comment on the merge request.
const pageSize = 100

// maxPages stops paging on a server that keeps returning full pages. It is far
// above any real merge request and exists so a broken response cannot loop
// forever.
const maxPages = 100

// listPages follows the API's pagination, decoding each page into the result.
func listPages[T any](ctx context.Context, c *giteeClient, base string) ([]T, error) {
	var all []T
	for page := 1; page <= maxPages; page++ {
		query := url.Values{}
		query.Set("page", strconv.Itoa(page))
		query.Set("per_page", strconv.Itoa(pageSize))
		var current []T
		if err := c.do(ctx, http.MethodGet, base+"?"+query.Encode(), nil, &current); err != nil {
			return nil, err
		}
		all = append(all, current...)
		if len(current) < pageSize {
			return all, nil
		}
	}
	return all, nil
}

// ListPRFiles returns the changed files with their patches. The patch is what
// lets the poster translate a file line into the diff position Gitee expects,
// without cloning anything.
func (c *giteeClient) ListPRFiles(ctx context.Context, owner, repo string, number int) ([]pullRequestFile, error) {
	return listPages[pullRequestFile](ctx, c, fmt.Sprintf("%s/pulls/%d/files", repoPath(owner, repo), number))
}

// ListPRComments returns every comment on the merge request, oldest first as
// Gitee orders them. The summary comment is found among these by marker.
func (c *giteeClient) ListPRComments(ctx context.Context, owner, repo string, number int) ([]pullRequestComment, error) {
	return listPages[pullRequestComment](ctx, c, fmt.Sprintf("%s/pulls/%d/comments", repoPath(owner, repo), number))
}

// CreateLineComment posts a comment against a specific line of the diff.
//
// commit_id has to be the commit the diff was computed from, not the branch
// head: by the time the review finishes the author may have pushed again, and
// a position that was correct for one diff is meaningless against another.
func (c *giteeClient) CreateLineComment(ctx context.Context, owner, repo string, number int, body, commitID, path string, position int) (*pullRequestComment, error) {
	form := url.Values{}
	form.Set("body", body)
	form.Set("commit_id", commitID)
	form.Set("path", path)
	form.Set("position", strconv.Itoa(position))

	var out pullRequestComment
	endpoint := fmt.Sprintf("%s/pulls/%d/comments", repoPath(owner, repo), number)
	if err := c.do(ctx, http.MethodPost, endpoint, form, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateSummaryComment posts a comment with no path or position, which is how
// Gitee represents a comment on the merge request as a whole.
func (c *giteeClient) CreateSummaryComment(ctx context.Context, owner, repo string, number int, body string) (*pullRequestComment, error) {
	form := url.Values{}
	form.Set("body", body)

	var out pullRequestComment
	endpoint := fmt.Sprintf("%s/pulls/%d/comments", repoPath(owner, repo), number)
	if err := c.do(ctx, http.MethodPost, endpoint, form, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateComment rewrites a comment in place. This is what keeps the summary
// from accumulating one near-identical block per push.
func (c *giteeClient) UpdateComment(ctx context.Context, owner, repo string, commentID int64, body string) error {
	form := url.Values{}
	form.Set("body", body)
	endpoint := fmt.Sprintf("%s/pulls/comments/%d", repoPath(owner, repo), commentID)
	return c.do(ctx, http.MethodPatch, endpoint, form, nil)
}
