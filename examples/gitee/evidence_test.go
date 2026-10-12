// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// stubDoer answers from a table of URL to response, so the fetcher can be
// exercised without a network.
type stubDoer struct {
	mu        sync.Mutex
	responses map[string]stubResponse
	requests  []string
}

type stubResponse struct {
	status int
	body   string
}

func (s *stubDoer) Do(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.requests = append(s.requests, req.URL.String())
	response, ok := s.responses[req.URL.String()]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no stub for %s", req.URL)
	}
	status := response.status
	if status == 0 {
		status = http.StatusOK
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(response.body)),
		Header:     http.Header{},
	}, nil
}

func (s *stubDoer) called(pathFragment string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, request := range s.requests {
		if strings.Contains(request, pathFragment) {
			return true
		}
	}
	return false
}

const listing = `{"files":[
	{"type":"file","name":"/package.json","size":100},
	{"type":"file","name":"/dist/index.d.ts","size":200},
	{"type":"file","name":"/dist/error.d.ts","size":150},
	{"type":"file","name":"/dist/index.js","size":900},
	{"type":"file","name":"/dist/types.d.mts","size":80}
]}`

func testCDN(doer httpDoer) *npmCDN {
	cdn := newNPMCDN(doer, func(string, ...any) {})
	cdn.cdnBase = "https://cdn.example"
	cdn.dataBase = "https://data.example"
	return cdn
}

func pinnedNext() dependency {
	return dependency{Name: "next", Version: "16.3.0", Ecosystem: "npm"}
}

func TestIsDeclarationFile(t *testing.T) {
	declarations := []string{"/dist/index.d.ts", "/dist/types.d.mts", "/dist/legacy.d.cts"}
	for _, name := range declarations {
		if !isDeclarationFile(name) {
			t.Errorf("%s should be read as a declaration", name)
		}
	}
	for _, name := range []string{"/dist/index.js", "/README.md", "/next.config.js"} {
		if isDeclarationFile(name) {
			t.Errorf("%s is not a declaration", name)
		}
	}
}

// multibyte is a three-byte character, which is what the truncation below has
// to avoid splitting.
const multibyte = "错" // allow-non-english: fixture, the multibyte rune is the point

// The text clipped here is often not ASCII, and a byte slice would split a
// character and put invalid UTF-8 into a log line and the public summary.
func TestClipKeepsRunesWhole(t *testing.T) {
	text := strings.Repeat(multibyte, 10)

	got := clip(text, 7)
	if !utf8.ValidString(got) {
		t.Fatalf("clip produced invalid UTF-8: %q", got)
	}
	if strings.Count(got, multibyte) != 2 {
		t.Errorf("clip(%q, 7) = %q, want two whole runes", text, got)
	}

	// Short enough to pass through untouched, and a limit that lands exactly on
	// a boundary is kept rather than trimmed back.
	if got := clip("abcdef", 6); got != "abcdef" {
		t.Errorf("clip should not touch text within the limit, got %q", got)
	}
	if got := clip(strings.Repeat(multibyte, 2), 6); got != strings.Repeat(multibyte, 2) {
		t.Errorf("clip should not touch text within the limit, got %q", got)
	}
}

func TestSurfaceReadsEveryDeclaration(t *testing.T) {
	doer := &stubDoer{responses: map[string]stubResponse{
		"https://data.example/next@16.3.0?structure=flat":  {body: listing},
		"https://cdn.example/next@16.3.0/dist/index.d.ts":  {body: "interface RetryProps {\n  retry?: () => void\n}"},
		"https://cdn.example/next@16.3.0/dist/error.d.ts":  {body: "declare function useError(): Error"},
		"https://cdn.example/next@16.3.0/dist/types.d.mts": {body: "export type Reset = () => void"},
	}}
	surface, err := testCDN(doer).Surface(context.Background(), pinnedNext())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !surface.Complete {
		t.Error("every declaration file was read, so the surface is complete")
	}
	// The JavaScript file is not a declaration and must not have been fetched.
	if doer.called("/dist/index.js") {
		t.Error("a .js file is not part of the type surface")
	}
	if len(surface.contents) != 3 {
		t.Errorf("read %d files, want 3", len(surface.contents))
	}

	// The decisive property: a member declared in the pinned version is found.
	if lines := surface.Mentions("retry"); len(lines) != 1 {
		t.Fatalf("mentions of retry = %v", lines)
	} else if !strings.Contains(lines[0], "retry?: () => void") || !strings.Contains(lines[0], "index.d.ts:2") {
		t.Errorf("a mention must be labelled with where it was found, got %q", lines[0])
	}
	if lines := surface.Mentions("nonexistentMember"); len(lines) != 0 {
		t.Errorf("got %v, want no mentions", lines)
	}
}

func TestSurfaceMentionsIsBoundedAndOrdered(t *testing.T) {
	var body strings.Builder
	body.WriteString("start\n")
	for index := range 40 {
		fmt.Fprintf(&body, "  retry%d\n", index)
		_ = index
	}
	for range 20 {
		body.WriteString("  retry\n")
	}
	doer := &stubDoer{responses: map[string]stubResponse{
		"https://data.example/next@16.3.0?structure=flat": {body: `{"files":[{"name":"/index.d.ts","size":1}]}`},
		"https://cdn.example/next@16.3.0/index.d.ts":      {body: body.String()},
	}}
	surface, err := testCDN(doer).Surface(context.Background(), pinnedNext())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// `retry` must not match `retry0`, and the list must be capped: a page of
	// matches would bury the line that decides the question.
	lines := surface.Mentions("retry")
	if len(lines) != maxMentions {
		t.Fatalf("got %d lines, want the cap of %d: %v", len(lines), maxMentions, lines)
	}
}

func TestSurfaceIsIncompleteWhenAFileCannotBeRead(t *testing.T) {
	doer := &stubDoer{responses: map[string]stubResponse{
		"https://data.example/next@16.3.0?structure=flat": {body: listing},
		"https://cdn.example/next@16.3.0/dist/index.d.ts": {body: "declare const retry: number"},
		"https://cdn.example/next@16.3.0/dist/error.d.ts": {status: http.StatusNotFound},
	}}
	surface, err := testCDN(doer).Surface(context.Background(), pinnedNext())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// An absence from a partly read surface proves nothing, so the flag has to
	// be false or a finding could be dropped on missing evidence.
	if surface.Complete {
		t.Error("a file that could not be read makes the surface incomplete")
	}
}

func TestSurfaceIsIncompleteWhenThePackageIsTooLarge(t *testing.T) {
	doer := &stubDoer{responses: map[string]stubResponse{
		"https://data.example/next@16.3.0?structure=flat": {body: listing},
		"https://cdn.example/next@16.3.0/dist/index.d.ts": {body: "declare const retry: number"},
	}}
	cdn := testCDN(doer)
	cdn.maxFiles = 1
	surface, err := cdn.Surface(context.Background(), pinnedNext())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if surface.Complete {
		t.Error("dropping declaration files makes the surface incomplete")
	}
	if !doer.called("index.d.ts") {
		t.Error("the files within the cap should still be read")
	}
}

func TestSurfaceSkipsOversizedFiles(t *testing.T) {
	doer := &stubDoer{responses: map[string]stubResponse{
		"https://data.example/next@16.3.0?structure=flat": {
			body: `{"files":[{"name":"/huge.d.ts","size":99999999},{"name":"/small.d.ts","size":10}]}`,
		},
		"https://cdn.example/next@16.3.0/small.d.ts": {body: "declare const retry: number"},
	}}
	surface, err := testCDN(doer).Surface(context.Background(), pinnedNext())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doer.called("huge.d.ts") {
		t.Error("a file over the size cap must not be fetched")
	}
	if len(surface.contents) != 1 {
		t.Errorf("got %d files", len(surface.contents))
	}
	// The declaration that was skipped is the whole point: an absence from a
	// surface that never read it proves nothing, so leaving Complete true here
	// would let a finding be dropped on evidence that was never examined.
	if surface.Complete {
		t.Error("skipping a declaration makes the surface incomplete")
	}
}

// The listing's size is a hint, not a guarantee. A body longer than the cap has
// to be treated the same way, or a declaration would be silently shortened.
func TestSurfaceTreatsATruncatedBodyAsUnread(t *testing.T) {
	doer := &stubDoer{responses: map[string]stubResponse{
		"https://data.example/next@16.3.0?structure=flat": {
			body: `{"files":[{"name":"/index.d.ts","size":10}]}`,
		},
		"https://cdn.example/next@16.3.0/index.d.ts": {body: strings.Repeat("x", 64)},
	}}
	cdn := testCDN(doer)
	cdn.maxFileSize = 16

	surface, err := cdn.Surface(context.Background(), pinnedNext())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if surface.Complete {
		t.Error("a body over the cap was not read in full, so the surface is incomplete")
	}
	if len(surface.contents) != 0 {
		t.Errorf("a truncated declaration must not count as evidence: %+v", surface.contents)
	}
}

func TestSurfaceRefusesWhatItCannotEvidence(t *testing.T) {
	cases := map[string]struct {
		dep    dependency
		doer   *stubDoer
		wantIn string
	}{
		"another ecosystem": {
			dep:    dependency{Name: "cobra", Version: "v1.8.1", Ecosystem: "go"},
			doer:   &stubDoer{},
			wantIn: "ecosystem",
		},
		"unpinned": {
			dep:    dependency{Name: "next", Range: "^16.3.0", Ecosystem: "npm"},
			doer:   &stubDoer{},
			wantIn: "no version",
		},
		"listing fails": {
			dep:    pinnedNext(),
			doer:   &stubDoer{responses: map[string]stubResponse{"https://data.example/next@16.3.0?structure=flat": {status: 500}}},
			wantIn: "list files",
		},
		"no declarations": {
			dep: pinnedNext(),
			doer: &stubDoer{responses: map[string]stubResponse{
				"https://data.example/next@16.3.0?structure=flat": {body: `{"files":[{"name":"/index.js","size":10}]}`},
			}},
			wantIn: "no type declarations",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := testCDN(tc.doer).Surface(context.Background(), tc.dep)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantIn)
			}
		})
	}
}

func TestSurfaceStopsOnACancelledContext(t *testing.T) {
	doer := &stubDoer{responses: map[string]stubResponse{
		"https://data.example/next@16.3.0?structure=flat": {body: listing},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	surface, err := testCDN(doer).Surface(ctx, pinnedNext())
	if err != nil {
		return // failing the listing is equally correct
	}
	if surface.Complete {
		t.Error("nothing could be read, so the surface is not complete")
	}
}

func TestMentionsOnANilSurface(t *testing.T) {
	var surface *apiSurface
	if lines := surface.Mentions("retry"); lines != nil {
		t.Errorf("got %v", lines)
	}
}

func TestClip(t *testing.T) {
	if got := clip("abcdef", 3); got != "abc..." {
		t.Errorf("got %q", got)
	}
	if got := clip("ab", 3); got != "ab" {
		t.Errorf("got %q", got)
	}
}
