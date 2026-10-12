// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// apiSurface is what a package publishes at one version, as far as this service
// looked at it.
type apiSurface struct {
	// Description names what was examined, for the log and the judge prompt.
	Description string
	// Complete reports whether every declaration file the package publishes was
	// read. This is the field that decides whether an absence means anything: a
	// member missing from a surface that was only partly read tells you nothing,
	// and a finding must never be dropped on that basis.
	Complete bool
	// contents maps a declaration file to its text.
	contents map[string]string
}

// maxMentions bounds how much of a surface is handed to the judge. A common
// word can be declared hundreds of times, and a page of matches buries the one
// line that decides the question.
const maxMentions = 8

// Mentions returns the declaration lines that name an identifier, each labelled
// with where it was found. An empty result on a Complete surface is evidence
// that the member does not exist at that version.
func (s *apiSurface) Mentions(identifier string) []string {
	if s == nil || identifier == "" {
		return nil
	}
	files := make([]string, 0, len(s.contents))
	for name := range s.contents {
		files = append(files, name)
	}
	// Sorted so that the same surface always yields the same lines: the judge's
	// verdict has to be reproducible from the log.
	sort.Strings(files)

	var found []string
	for _, name := range files {
		for number, line := range strings.Split(s.contents[name], "\n") {
			if indexWord(line, identifier, false) < 0 {
				continue
			}
			found = append(found, fmt.Sprintf("%s:%d: %s", name, number+1, strings.TrimSpace(clip(line, 400))))
			if len(found) >= maxMentions {
				return found
			}
		}
	}
	return found
}

func clip(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	// Cut on a rune boundary. The text clipped here is not always ASCII -- an
	// npm package's declarations carry the author's own prose, and a model's
	// reply is frequently Chinese -- and a byte slice would split a character,
	// putting invalid UTF-8 into a log line and into the public summary.
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "..."
}

// evidenceSource fetches the published surface of a pinned dependency.
type evidenceSource interface {
	Surface(ctx context.Context, dep dependency) (*apiSurface, error)
}

// httpDoer is the HTTP seam, so the fetcher can be tested without a network.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// npmCDN reads a package's published type declarations off jsDelivr.
//
// The published declarations are the right evidence for this class of finding
// and the lock file is the right version, but neither is reachable from the
// repository alone, which is exactly why the check has to happen out here. The
// package's own metadata supplies the file list, so completeness is knowable
// rather than assumed -- and completeness is what a claim of absence rests on.
type npmCDN struct {
	doer     httpDoer
	cdnBase  string
	dataBase string

	// maxFiles and maxBytes bound one lookup. Exceeding either marks the
	// surface incomplete, which disables absence conclusions rather than
	// producing a wrong one.
	maxFiles    int
	maxFileSize int64
	maxBytes    int64

	fetchConcurrency int
	logf             func(format string, args ...any)
}

func newNPMCDN(doer httpDoer, logf func(string, ...any)) *npmCDN {
	return &npmCDN{
		doer:             doer,
		cdnBase:          "https://cdn.jsdelivr.net/npm",
		dataBase:         "https://data.jsdelivr.com/v1/packages/npm",
		maxFiles:         400,
		maxFileSize:      4 << 20,
		maxBytes:         32 << 20,
		fetchConcurrency: 8,
		logf:             logf,
	}
}

// Surface fetches and reads every declaration file a package publishes.
func (d *npmCDN) Surface(ctx context.Context, dep dependency) (*apiSurface, error) {
	if dep.Ecosystem != "npm" {
		return nil, fmt.Errorf("no evidence source for the %s ecosystem", dep.Ecosystem)
	}
	if !dep.Exact() {
		return nil, fmt.Errorf("%s is pinned by the range %q, which names no version to check", dep.Name, dep.Range)
	}

	listed, err := d.fileList(ctx, dep)
	if err != nil {
		return nil, err
	}
	surface := &apiSurface{
		Description: fmt.Sprintf("type declarations published by %s@%s", dep.Name, dep.Version),
		Complete:    true,
		contents:    map[string]string{},
	}

	var declarations []string
	for _, file := range listed {
		if !isDeclarationFile(file.Name) {
			continue
		}
		if file.Size > d.maxFileSize {
			// A declaration that was not read cannot support the conclusion that
			// something is absent from the package, so the surface is not
			// complete. Marking it complete here would be the one way an absence
			// could drop a finding on evidence that was never examined.
			d.logf("evidence: %s@%s %s is %d bytes, over the per-file cap; treating the surface as incomplete",
				dep.Name, dep.Version, file.Name, file.Size)
			surface.Complete = false
			continue
		}
		declarations = append(declarations, file.Name)
	}

	if len(declarations) == 0 {
		// A package with no declarations cannot be checked by this means, and
		// saying so is better than treating its silence as evidence.
		return nil, fmt.Errorf("%s@%s publishes no type declarations", dep.Name, dep.Version)
	}
	if len(declarations) > d.maxFiles {
		surface.Complete = false
		declarations = declarations[:d.maxFiles]
	}

	var total int64
	fetched, failures := d.fetchAll(ctx, dep, declarations, &total)
	if failures > 0 {
		surface.Complete = false
	}
	for name, body := range fetched {
		surface.contents[name] = body
	}
	if total > d.maxBytes {
		surface.Complete = false
	}
	return surface, nil
}

// fetchAll reads the declarations concurrently, bounded by fetchConcurrency and
// by the byte budget.
func (d *npmCDN) fetchAll(ctx context.Context, dep dependency, paths []string, total *int64) (map[string]string, int) {
	var mu sync.Mutex
	fetched := make(map[string]string, len(paths))
	var failures int64

	sem := make(chan struct{}, d.fetchConcurrency)
	var wg sync.WaitGroup
	for _, filePath := range paths {
		wg.Add(1)
		go func(filePath string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				atomic.AddInt64(&failures, 1)
				return
			}
			defer func() { <-sem }()

			// Checked before each fetch rather than after, so a package far over
			// the budget stops costing requests once the budget is spent.
			if atomic.LoadInt64(total) > d.maxBytes {
				atomic.AddInt64(&failures, 1)
				return
			}
			body, err := d.read(ctx, dep, filePath)
			if err != nil {
				d.logf("evidence: %s@%s %s: %v", dep.Name, dep.Version, filePath, err)
				atomic.AddInt64(&failures, 1)
				return
			}
			atomic.AddInt64(total, int64(len(body)))
			mu.Lock()
			fetched[filePath] = string(body)
			mu.Unlock()
		}(filePath)
	}
	wg.Wait()
	return fetched, int(failures)
}

// jsdelivrListing is the package metadata the data API returns.
type jsdelivrListing struct {
	Files []listedFile `json:"files"`
}

// listedFile is one published file. The size is carried rather than used to
// filter here, because whether a file is too large to read only matters for the
// declarations -- and skipping one of those silently is what would let an
// incomplete surface claim to be complete.
type listedFile struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

func (d *npmCDN) fileList(ctx context.Context, dep dependency) ([]listedFile, error) {
	var listing jsdelivrListing
	endpoint := fmt.Sprintf("%s/%s@%s?structure=flat", d.dataBase, dep.Name, dep.Version)
	if err := d.getJSON(ctx, endpoint, &listing); err != nil {
		return nil, fmt.Errorf("list files of %s@%s: %w", dep.Name, dep.Version, err)
	}
	return listing.Files, nil
}

// isDeclarationFile reports whether a published file is a type declaration.
// Their extensions are the ones TypeScript resolves a declaration from, so
// anything else is source that the published package may not even contain.
func isDeclarationFile(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".d.ts") ||
		strings.HasSuffix(lower, ".d.mts") ||
		strings.HasSuffix(lower, ".d.cts")
}

func (d *npmCDN) read(ctx context.Context, dep dependency, filePath string) ([]byte, error) {
	endpoint := fmt.Sprintf("%s/%s@%s%s", d.cdnBase, dep.Name, dep.Version, filePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := d.doer.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	// One byte past the cap is read so that a truncated file can be told apart
	// from one that exactly fills it. A silently shortened declaration is an
	// absence of evidence, and an absence that looks like a full read is the
	// one thing this surface must never hand the judge.
	body, err := io.ReadAll(io.LimitReader(resp.Body, d.maxFileSize+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > d.maxFileSize {
		return nil, fmt.Errorf("%d bytes, over the %d byte cap", len(body), d.maxFileSize)
	}
	return body, nil
}

// maxListingBytes bounds the package metadata document. It is separate from
// maxFileSize because the two measure different things: the listing is one JSON
// index, and a declaration is one file out of the package.
const maxListingBytes = 8 << 20

func (d *npmCDN) getJSON(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := d.doer.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxListingBytes)).Decode(out)
}

// newEvidenceHTTPClient is the client the service uses in production. The
// timeout covers one declaration file, and the fetcher's own context bounds the
// lookup as a whole.
func newEvidenceHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}
