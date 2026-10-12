// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testClient points a client at a stub server. The backoff is zeroed so the
// retry tests assert the schedule rather than sit through it.
func testClient(t *testing.T, handler http.HandlerFunc) (*giteeClient, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &giteeClient{
		base:       server.URL,
		token:      "test-token",
		client:     &http.Client{Timeout: 5 * time.Second},
		maxRetries: 3,
		backoff:    0,
	}, server
}

func TestPatchFieldAcceptsBothSpellings(t *testing.T) {
	cases := map[string]string{
		"bare string":  `"@@ -1 +1 @@"`,
		"object":       `{"diff":"@@ -1 +1 @@"}`,
		"null":         `null`,
		"empty object": `{}`,
		"empty string": `""`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			var p patchField
			if err := json.Unmarshal([]byte(raw), &p); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}

	// Gitee returns an object on the v5 API; GitHub and the Gitee examples
	// return a string. Getting this wrong loses every inline position.
	var asObject patchField
	if err := json.Unmarshal([]byte(`{"diff":"@@ -1 +1 @@"}`), &asObject); err != nil {
		t.Fatal(err)
	}
	if asObject.Diff != "@@ -1 +1 @@" {
		t.Errorf("object form lost the diff: %q", asObject.Diff)
	}
	var asString patchField
	if err := json.Unmarshal([]byte(`"@@ -1 +1 @@"`), &asString); err != nil {
		t.Fatal(err)
	}
	if asString.Diff != "@@ -1 +1 @@" {
		t.Errorf("string form lost the diff: %q", asString.Diff)
	}
}

func TestPatchFieldRejectsGarbage(t *testing.T) {
	var p patchField
	if err := json.Unmarshal([]byte(`123`), &p); err == nil {
		t.Fatal("want an error for a patch that is neither a string nor an object")
	}
}

func TestListPRFiles(t *testing.T) {
	client, server := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widget/pulls/7/files" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "token test-token" {
			t.Errorf("Authorization = %q", got)
		}
		// The token must not travel in the query string, where it would be
		// captured by access logs on every hop.
		if strings.Contains(r.URL.RawQuery, "test-token") {
			t.Error("the token must not appear in the query string")
		}
		_, _ = w.Write([]byte(`[
			{"filename":"a.ts","sha":"s1","status":"modified","patch":{"diff":"@@ -1 +1 @@"}},
			{"filename":"b.ts","sha":"s2","status":"added","patch":"@@ -2 +2 @@"}
		]`))
	})
	_ = server

	files, err := client.ListPRFiles(context.Background(), "acme", "widget", 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("got %d files", len(files))
	}
	if files[0].Patch.Diff != "@@ -1 +1 @@" || files[1].Patch.Diff != "@@ -2 +2 @@" {
		t.Fatalf("patches not decoded: %+v", files)
	}
}

func TestListPRComments(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"id":1,"path":"a.ts","position":null,"new_line":42,"body":"hi",
			 "comment_type":"diff_comment","user":{"login":"someone"}},
			{"id":2,"body":"summary","comment_type":"pr_comment","user":{"login":"me"}}
		]`))
	})
	comments, err := client.ListPRComments(context.Background(), "acme", "widget", 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("got %d comments", len(comments))
	}
	if comments[0].NewLine == nil || *comments[0].NewLine != 42 {
		t.Errorf("new_line not decoded: %+v", comments[0])
	}
	if comments[0].Position != nil {
		t.Errorf("a null position must stay nil, got %v", *comments[0].Position)
	}
	if comments[1].User.Login != "me" {
		t.Errorf("user not decoded: %+v", comments[1])
	}
}

func TestCreateLineCommentSendsThePositionForm(t *testing.T) {
	var got map[string]string
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		got = map[string]string{
			"body": r.PostForm.Get("body"), "commit_id": r.PostForm.Get("commit_id"),
			"path": r.PostForm.Get("path"), "position": r.PostForm.Get("position"),
		}
		_, _ = w.Write([]byte(`{"id":99,"path":"a.ts","new_line":42,"position":5}`))
	})

	comment, err := client.CreateLineComment(context.Background(), "acme", "widget", 7, "a finding", "deadbeef", "a.ts", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]string{"body": "a finding", "commit_id": "deadbeef", "path": "a.ts", "position": "5"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("form %s = %q, want %q", k, got[k], v)
		}
	}
	// The response is what the runtime position self-check reads back.
	if comment.NewLine == nil || *comment.NewLine != 42 {
		t.Errorf("response not decoded: %+v", comment)
	}
}

func TestCreateSummaryCommentOmitsTheLineFields(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"path", "position", "commit_id"} {
			if v := r.PostForm.Get(field); v != "" {
				t.Errorf("a summary comment must not set %s, got %q", field, v)
			}
		}
		_, _ = w.Write([]byte(`{"id":100,"body":"summary"}`))
	})
	if _, err := client.CreateSummaryComment(context.Background(), "acme", "widget", 7, "summary"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestUpdateCommentUsesPatch(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		if r.URL.Path != "/repos/acme/widget/pulls/comments/55" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if got := r.PostForm.Get("body"); got != "rewritten" {
			t.Errorf("body = %q", got)
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := client.UpdateComment(context.Background(), "acme", "widget", 55, "rewritten"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRetriesTransientFailures(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusTooManyRequests, http.StatusRequestTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var attempts int32
			client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if atomic.AddInt32(&attempts, 1) == 1 {
					w.WriteHeader(status)
					return
				}
				_, _ = w.Write([]byte(`[]`))
			})
			if _, err := client.ListPRComments(context.Background(), "acme", "widget", 1); err != nil {
				t.Fatalf("a transient %d should be retried: %v", status, err)
			}
			if got := atomic.LoadInt32(&attempts); got != 2 {
				t.Errorf("attempts = %d, want 2", got)
			}
		})
	}
}

// A 404 or 401 is not going to become a 200, and retrying it just delays the
// error the operator needs to see.
func TestDoesNotRetryPermanentFailures(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var attempts int32
			client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&attempts, 1)
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"nope"}`))
			})
			_, err := client.ListPRComments(context.Background(), "acme", "widget", 1)
			if err == nil {
				t.Fatalf("want an error for %d", status)
			}
			if got := atomic.LoadInt32(&attempts); got != 1 {
				t.Errorf("attempts = %d, want 1 (no retry)", got)
			}
			if !strings.Contains(err.Error(), "nope") {
				t.Errorf("the API's own message should surface, got: %v", err)
			}
		})
	}
}

func TestRetriesAreBounded(t *testing.T) {
	var attempts int32
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	})
	if _, err := client.ListPRComments(context.Background(), "acme", "widget", 1); err == nil {
		t.Fatal("want an error once the retries are exhausted")
	}
	if got := atomic.LoadInt32(&attempts); got != 4 {
		t.Errorf("attempts = %d, want the initial try plus 3 retries", got)
	}
}

func TestDecodeFailureIsReported(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{not json`))
	})
	if _, err := client.ListPRComments(context.Background(), "acme", "widget", 1); err == nil {
		t.Fatal("want a decode error")
	} else if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error should say it was a decode failure, got: %v", err)
	}
}

// A body over the cap is truncated by the reader, and handing the fragment to
// the decoder would report it as malformed JSON -- hiding the actual cause from
// whoever has to diagnose it.
func TestOverCapResponseIsReportedAsOverCap(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[" + strings.Repeat("0", maxResponseBytes) + "]"))
	})
	err := client.UpdateComment(context.Background(), "acme", "widget", 1, "body")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("error should name the size limit, got: %v", err)
	}
	if strings.Contains(err.Error(), "decode") {
		t.Errorf("a size problem should not be reported as a decode failure: %v", err)
	}
}

func TestEmptyResponseBodyIsNotADecodeFailure(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// UpdateComment decodes nothing; an empty 200 is a success.
	if err := client.UpdateComment(context.Background(), "acme", "widget", 1, "body"); err != nil {
		t.Fatalf("an empty 200 should succeed: %v", err)
	}
}

func TestCancelledContextStopsRetrying(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	client.backoff = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.ListPRComments(ctx, "acme", "widget", 1); err == nil {
		t.Fatal("want the context error")
	}
}

func TestGiteeErrorTruncatesAHugeBody(t *testing.T) {
	err := &giteeError{Status: 500, Method: "POST", Path: "/x", Body: strings.Repeat("x", 5000)}
	message := err.Error()
	if len(message) > 500 {
		t.Fatalf("error message should be truncated, got %d chars", len(message))
	}
	if !strings.Contains(message, "status 500") {
		t.Errorf("message should carry the status, got: %s", message)
	}
}

func TestRepoPathEscapesOwnerAndName(t *testing.T) {
	if got := repoPath("a b", "c/d"); got != "/repos/a%20b/c%2Fd" {
		t.Fatalf("repoPath = %q", got)
	}
}

// A truncated list is not a smaller answer, it is a wrong one: the files left
// out lose their positions, and the comments left out take the sticky summary
// and the posted-markers with them, so the next run duplicates every comment on
// the merge request.
func TestListFollowsPagesUntilOneIsShort(t *testing.T) {
	full := make([]map[string]any, 0, pageSize)
	for index := range pageSize {
		full = append(full, map[string]any{
			"filename": fmt.Sprintf("file%03d.ts", index), "sha": "s", "status": "modified",
		})
	}
	encoded, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	var mu sync.Mutex
	var pages []string
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		mu.Lock()
		pages = append(pages, page)
		mu.Unlock()
		if r.URL.Query().Get("per_page") != strconv.Itoa(pageSize) {
			t.Errorf("per_page = %q", r.URL.Query().Get("per_page"))
		}
		if page == "1" {
			_, _ = w.Write(encoded)
			return
		}
		// The last page is short, which is what ends the walk.
		_, _ = w.Write([]byte(`[{"filename":"last.ts","sha":"s","status":"added"}]`))
	})

	files, err := client.ListPRFiles(context.Background(), "acme", "widget", 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != pageSize+1 {
		t.Fatalf("got %d files, want the whole list", len(files))
	}
	if files[len(files)-1].Filename != "last.ts" {
		t.Errorf("the last page was not appended: %+v", files[len(files)-1])
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(pages, ",") != "1,2" {
		t.Errorf("requested pages %v", pages)
	}
}

// A server that keeps answering with full pages must not spin forever.
func TestListStopsAtThePageCeiling(t *testing.T) {
	full := make([]map[string]any, 0, pageSize)
	for index := range pageSize {
		full = append(full, map[string]any{"filename": fmt.Sprintf("f%d.ts", index), "sha": "s", "status": "modified"})
	}
	encoded, _ := json.Marshal(full)

	var requests atomic.Int64
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write(encoded)
	})

	if _, err := client.ListPRFiles(context.Background(), "acme", "widget", 7); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := requests.Load(); got != maxPages {
		t.Errorf("made %d requests, want the ceiling of %d", got, maxPages)
	}
}

// A page that fails is an error, not a short list: folding the findings would
// hide the failure behind a review that looks complete.
func TestListReportsAFailingPage(t *testing.T) {
	client, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	})
	if _, err := client.ListPRComments(context.Background(), "acme", "widget", 7); err == nil {
		t.Fatal("want an error")
	}
}
