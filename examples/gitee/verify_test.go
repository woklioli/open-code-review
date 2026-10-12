// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeEvidence answers with a prepared surface.
type fakeEvidence struct {
	surfaces map[string]*apiSurface
	err      error
	lookups  []string
}

func (f *fakeEvidence) Surface(_ context.Context, dep dependency) (*apiSurface, error) {
	f.lookups = append(f.lookups, dep.Name+"@"+dep.Version)
	if f.err != nil {
		return nil, f.err
	}
	surface, ok := f.surfaces[dep.Name+"@"+dep.Version]
	if !ok {
		return nil, errors.New("no surface prepared for " + dep.Name)
	}
	return surface, nil
}

// fakeJudge answers from a function, so a test can assert what it was asked.
type fakeJudge struct {
	answer func(verifyRequest) (verdict, error)
	asked  []verifyRequest
}

func (f *fakeJudge) Judge(_ context.Context, request verifyRequest) (verdict, error) {
	f.asked = append(f.asked, request)
	if f.answer == nil {
		return verdict{}, errors.New("no answer prepared")
	}
	return f.answer(request)
}

// completeSurface is a pinned package whose declarations mention `retry`.
func completeSurface() *apiSurface {
	return &apiSurface{
		Description: "type declarations published by next@16.3.0",
		Complete:    true,
		contents: map[string]string{
			"index.d.ts": "interface RetryProps {\n  retry?: () => void\n}",
		},
	}
}

func verifierFor(files fakeFiles, evidence evidenceSource, decision judge) *dependencyVerifier {
	return &dependencyVerifier{source: files, evidence: evidence, judge: decision, logf: func(string, ...any) {}}
}

func nextPinnedFiles() fakeFiles {
	return fakeFiles{"package-lock.json": `{"packages": {"node_modules/next": {"version": "16.3.0"}}}`}
}

// This is the regression the whole round exists for: the measured false
// positive is dropped, because the pinned version's declarations do declare the
// member the finding says does not exist.
func TestVerifyDropsTheMeasuredFalsePositive(t *testing.T) {
	evidence := &fakeEvidence{surfaces: map[string]*apiSurface{"next@16.3.0": completeSurface()}}
	decision := &fakeJudge{answer: func(verifyRequest) (verdict, error) {
		return verdict{Contradicted: true, Reason: "`retry` is declared in index.d.ts"}, nil
	}}
	report := &ocrReport{Comments: []ocrComment{{Path: "app/error.tsx", Content: retryPropFinding}}}

	options := verifierFor(nextPinnedFiles(), evidence, decision).Verify(context.Background(), report)

	if len(report.Comments) != 0 {
		t.Fatalf("the finding should have been removed, got %+v", report.Comments)
	}
	if options.Dropped != 1 {
		t.Errorf("Dropped = %d, want 1", options.Dropped)
	}
	if len(decision.asked) != 1 {
		t.Fatalf("the judge was asked %d times", len(decision.asked))
	}
	// The evidence has to reach the judge, or it is answering from memory.
	request := decision.asked[0]
	if request.DependencyName != "next" || request.DependencyVersion != "16.3.0" {
		t.Errorf("request = %+v", request)
	}
	if !request.SurfaceComplete {
		t.Error("the surface was complete and the judge should be told so")
	}
	var retry memberEvidence
	for _, member := range request.Members {
		if member.Name == "retry" {
			retry = member
		}
	}
	if len(retry.Lines) == 0 {
		t.Errorf("the `retry` declaration should have been handed over: %+v", request.Members)
	}
	if !strings.Contains(request.Finding, "there is no `retry` prop") {
		t.Errorf("the finding should be quoted verbatim: %q", request.Finding)
	}
}

// A verdict that says "contradicted" but does not name the member it found is
// not a verdict about this evidence, and must not remove a finding.
func TestVerifyIgnoresAnUnjustifiedVerdict(t *testing.T) {
	evidence := &fakeEvidence{surfaces: map[string]*apiSurface{"next@16.3.0": completeSurface()}}
	decision := &fakeJudge{answer: func(verifyRequest) (verdict, error) {
		return verdict{Contradicted: true, Reason: "the evidence contradicts the finding"}, nil
	}}
	report := &ocrReport{Comments: []ocrComment{{Content: retryPropFinding}}}

	options := verifierFor(nextPinnedFiles(), evidence, decision).Verify(context.Background(), report)

	if len(report.Comments) != 1 {
		t.Fatal("a verdict that cites nothing must not remove a finding")
	}
	if options.Dropped != 0 {
		t.Errorf("Dropped = %d", options.Dropped)
	}
}

func TestVerifyKeepsWhatTheJudgeClears(t *testing.T) {
	evidence := &fakeEvidence{surfaces: map[string]*apiSurface{"next@16.3.0": completeSurface()}}
	decision := &fakeJudge{answer: func(verifyRequest) (verdict, error) {
		return verdict{Contradicted: false, Reason: "the finding is about a different matter"}, nil
	}}
	report := &ocrReport{Comments: []ocrComment{{Content: retryPropFinding}}}

	if options := verifierFor(nextPinnedFiles(), evidence, decision).Verify(context.Background(), report); options.Dropped != 0 {
		t.Errorf("dropped %d", options.Dropped)
	}
	if len(report.Comments) != 1 {
		t.Fatal("the finding should have been kept")
	}
}

// When the pinned package does not mention any of the identifiers the finding
// names, the package cannot contradict it -- and asking would spend a call to
// be told the finding stands.
func TestVerifyDoesNotAskWhenNothingWasFound(t *testing.T) {
	evidence := &fakeEvidence{surfaces: map[string]*apiSurface{"next@16.3.0": {
		Description: "type declarations published by next@16.3.0",
		Complete:    true,
		contents:    map[string]string{"index.d.ts": "declare const unrelated: number"},
	}}}
	decision := &fakeJudge{}
	report := &ocrReport{Comments: []ocrComment{{Content: retryPropFinding}}}

	verifierFor(nextPinnedFiles(), evidence, decision).Verify(context.Background(), report)

	if len(decision.asked) != 0 {
		t.Errorf("the judge should not have been asked: %+v", decision.asked)
	}
	if len(report.Comments) != 1 {
		t.Fatal("the finding should have been kept")
	}
}

// Every failure keeps the finding: verification is an addition, and one that
// could not run must leave the review exactly as ocr produced it.
func TestVerifyKeepsEverythingWhenItCannotRun(t *testing.T) {
	populated := &fakeEvidence{surfaces: map[string]*apiSurface{"next@16.3.0": completeSurface()}}
	report := func() *ocrReport {
		return &ocrReport{Comments: []ocrComment{{Content: retryPropFinding}}}
	}

	t.Run("no judge configured", func(t *testing.T) {
		got := report()
		options := verifierFor(nextPinnedFiles(), populated, nil).Verify(context.Background(), got)
		if len(got.Comments) != 1 || options.Dropped != 0 || len(options.Notes) != 0 {
			t.Fatalf("got %+v, options %+v", got.Comments, options)
		}
	})
	t.Run("no manifests at all", func(t *testing.T) {
		got := report()
		decision := &fakeJudge{}
		options := verifierFor(fakeFiles{}, populated, decision).Verify(context.Background(), got)
		if len(got.Comments) != 1 || options.Dropped != 0 {
			t.Fatalf("got %+v", got.Comments)
		}
		if len(decision.asked) != 0 {
			t.Error("nothing could be pinned, so nothing could be checked")
		}
	})
	t.Run("evidence unavailable", func(t *testing.T) {
		got := report()
		evidence := &fakeEvidence{err: errors.New("registry unreachable")}
		decision := &fakeJudge{}
		options := verifierFor(nextPinnedFiles(), evidence, decision).Verify(context.Background(), got)
		if len(got.Comments) != 1 || options.Dropped != 0 {
			t.Fatalf("got %+v", got.Comments)
		}
		if len(decision.asked) != 0 {
			t.Error("there was no evidence to judge")
		}
	})
	t.Run("judge fails", func(t *testing.T) {
		got := report()
		decision := &fakeJudge{answer: func(verifyRequest) (verdict, error) {
			return verdict{}, errors.New("model timeout")
		}}
		options := verifierFor(nextPinnedFiles(), populated, decision).Verify(context.Background(), got)
		if len(got.Comments) != 1 || options.Dropped != 0 {
			t.Fatalf("got %+v", got.Comments)
		}
		// The note matters: a reader who knows a check ran should not read
		// silence as a pass.
		if len(options.Notes) != 1 || !strings.Contains(options.Notes[0], "model timeout") {
			t.Errorf("notes = %v", options.Notes)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		got := report()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		decision := &fakeJudge{}
		verifierFor(nextPinnedFiles(), populated, decision).Verify(ctx, got)
		if len(got.Comments) != 1 {
			t.Fatal("a cancelled round must not remove findings")
		}
	})
}

// A package looked up once serves every finding that names it.
func TestVerifyLooksUpEachPackageOnce(t *testing.T) {
	evidence := &fakeEvidence{surfaces: map[string]*apiSurface{"next@16.3.0": completeSurface()}}
	decision := &fakeJudge{answer: func(verifyRequest) (verdict, error) {
		return verdict{Contradicted: true, Reason: "`retry` is declared"}, nil
	}}
	report := &ocrReport{Comments: []ocrComment{
		{Path: "a.tsx", Content: retryPropFinding},
		{Path: "b.tsx", Content: "Next.js has no `retry` prop. " + retryPropFinding},
	}}

	options := verifierFor(nextPinnedFiles(), evidence, decision).Verify(context.Background(), report)

	if len(evidence.lookups) != 1 {
		t.Errorf("looked up %v, want one lookup", evidence.lookups)
	}
	if options.Dropped != 2 {
		t.Errorf("Dropped = %d, want 2", options.Dropped)
	}
}

// A package that could not be read is not re-fetched for the next finding.
func TestVerifyRemembersAFailedLookup(t *testing.T) {
	evidence := &fakeEvidence{}
	decision := &fakeJudge{}
	report := &ocrReport{Comments: []ocrComment{
		{Content: retryPropFinding},
		{Content: "Next.js also lacks the `reset` prop."},
	}}

	verifierFor(nextPinnedFiles(), evidence, decision).Verify(context.Background(), report)

	if len(evidence.lookups) != 1 {
		t.Errorf("looked up %v, want the failure to be remembered", evidence.lookups)
	}
}

// A finding the evidence does not mention is left alone, and only the ones the
// evidence does mention are put to the judge.
func TestVerifyOnlyJudgesWhatTheEvidenceMentions(t *testing.T) {
	evidence := &fakeEvidence{surfaces: map[string]*apiSurface{"next@16.3.0": completeSurface()}}
	decision := &fakeJudge{answer: func(verifyRequest) (verdict, error) {
		return verdict{Contradicted: false, Reason: "not contradicted"}, nil
	}}
	report := &ocrReport{Comments: []ocrComment{
		{Content: "Next.js has no `retry` prop."},                   // retry is declared
		{Content: "Next.js has no `useSomethingElse` prop at all."}, // not declared
	}}

	verifierFor(nextPinnedFiles(), evidence, decision).Verify(context.Background(), report)

	if len(decision.asked) != 1 {
		t.Fatalf("asked %d times, want 1: %+v", len(decision.asked), decision.asked)
	}
	if !strings.Contains(decision.asked[0].Finding, "retry") {
		t.Errorf("the wrong finding was judged: %q", decision.asked[0].Finding)
	}
}

func TestVerifyWithAnEmptyReport(t *testing.T) {
	decision := &fakeJudge{}
	report := &ocrReport{}
	if options := verifierFor(nextPinnedFiles(), &fakeEvidence{}, decision).Verify(context.Background(), report); options.Dropped != 0 {
		t.Errorf("got %+v", options)
	}
	if len(decision.asked) != 0 {
		t.Error("there was nothing to check")
	}
}

func TestVerifyWithANilReport(t *testing.T) {
	decision := &fakeJudge{}
	if options := verifierFor(nextPinnedFiles(), &fakeEvidence{}, decision).Verify(context.Background(), nil); options.Dropped != 0 {
		t.Errorf("got %+v", options)
	}
}

// A verdict is only allowed to remove a finding on the strength of a named
// line the evidence actually contains.
func TestDropJustified(t *testing.T) {
	members := []memberEvidence{
		{Name: "error", Lines: []string{"error.d.ts:1: declare function useError(): Error"}},
		{Name: "retry", Lines: []string{"index.d.ts:2: retry?: () => void"}},
		{Name: "absent"}, // found nowhere, so it cannot justify anything
	}
	cases := map[string]struct {
		result verdict
		want   bool
	}{
		"cites a found member":       {verdict{true, "`retry` is declared"}, true},
		"cites another found member": {verdict{true, "error exists in error.d.ts"}, true},
		"not contradicted":           {verdict{false, "`retry` is declared"}, false},
		"no reason":                  {verdict{true, ""}, false},
		"whitespace reason":          {verdict{true, "   "}, false},
		"cites a member never found": {verdict{true, "`absent` is declared"}, false},
		"cites nothing specific":     {verdict{true, "the evidence disagrees"}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := dropJustified(tc.result, members); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDropJustifiedWithNoMembers(t *testing.T) {
	if dropJustified(verdict{true, "anything"}, nil) {
		t.Error("nothing was found, so nothing can justify a drop")
	}
}

func TestSurfaceForCachesFailures(t *testing.T) {
	evidence := &fakeEvidence{}
	verifier := verifierFor(nextPinnedFiles(), evidence, &fakeJudge{})
	cache := map[string]*apiSurface{}
	dep := dependency{Name: "next", Version: "16.3.0", Ecosystem: "npm"}

	if _, err := verifier.surfaceFor(context.Background(), dep, cache); err == nil {
		t.Fatal("want an error")
	}
	if _, err := verifier.surfaceFor(context.Background(), dep, cache); err == nil {
		t.Fatal("want the cached error")
	}
	if len(evidence.lookups) != 1 {
		t.Errorf("looked up %d times, want the failure remembered", len(evidence.lookups))
	}

	// A successful lookup is cached too.
	evidence.surfaces = map[string]*apiSurface{"next@16.3.0": completeSurface()}
	cache = map[string]*apiSurface{}
	for range 2 {
		if _, err := verifier.surfaceFor(context.Background(), dep, cache); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if len(evidence.lookups) != 2 {
		t.Errorf("looked up %d times, want one more", len(evidence.lookups))
	}
}

func TestVerifyNotesNameTheFindingAndThePackage(t *testing.T) {
	evidence := &fakeEvidence{surfaces: map[string]*apiSurface{"next@16.3.0": completeSurface()}}
	decision := &fakeJudge{answer: func(verifyRequest) (verdict, error) {
		return verdict{}, fmt.Errorf("gateway returned 503")
	}}
	report := &ocrReport{Comments: []ocrComment{{Path: "app/error.tsx", StartLine: 4, Content: retryPropFinding}}}

	options := verifierFor(nextPinnedFiles(), evidence, decision).Verify(context.Background(), report)

	if len(options.Notes) != 1 {
		t.Fatalf("notes = %v", options.Notes)
	}
	for _, want := range []string{"app/error.tsx:4", "next@16.3.0", "503"} {
		if !strings.Contains(options.Notes[0], want) {
			t.Errorf("note %q should mention %q", options.Notes[0], want)
		}
	}
}
