// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"fmt"
	"strings"
)

// memberEvidence is what a package's published surface says about one
// identifier a finding names.
type memberEvidence struct {
	Name string
	// Lines are the declaration lines mentioning it. Empty on a complete
	// surface means the member does not exist at that version.
	Lines []string
}

// verifyRequest is one finding, and the evidence about the package it names.
type verifyRequest struct {
	Finding           string
	DependencyName    string
	DependencyVersion string
	Surface           string
	SurfaceComplete   bool
	Members           []memberEvidence
}

// dependencyVerifier removes the findings a pinned dependency contradicts.
//
// It exists to close exactly one gap. ocr already re-reads its own findings
// against the diff and removes the ones the diff refutes, and that filter is
// deliberately built to keep anything it cannot check -- "unverifiable is not
// incorrect". A finding about a package the diff does not contain is therefore
// kept by design, and the measured false positives were all of that kind. The
// missing input is the package itself, which is what this supplies.
type dependencyVerifier struct {
	// source reads the manifests out of the reviewed revision.
	source fileSource
	// evidence fetches the published surface of a pinned package.
	evidence evidenceSource
	// judge decides whether the surface contradicts the finding. A nil judge
	// keeps everything.
	judge judge
	logf  func(format string, args ...any)
}

// Verify removes the contradicted findings from the report and returns what the
// summary should say about the rest.
//
// Every failure keeps the finding. Verification is an addition to the review,
// and a verification that could not run must leave the review exactly as ocr
// produced it -- so a missing lock file, an unreachable registry, an
// unsupported ecosystem and a broken verdict all end the same way.
func (v *dependencyVerifier) Verify(ctx context.Context, report *ocrReport) postOptions {
	var options postOptions
	if v.judge == nil || report == nil || len(report.Comments) == 0 {
		return options
	}
	deps := loadDependencies(ctx, v.source)
	candidates := selectCandidates(report.Comments, deps)
	if len(candidates) == 0 {
		return options
	}
	v.logf("checking %d of %d findings against pinned dependencies", len(candidates), len(report.Comments))

	dropped := map[string]bool{}
	surfaces := map[string]*apiSurface{}

	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			break
		}
		dep := candidate.Dependency
		surface, err := v.surfaceFor(ctx, dep, surfaces)
		if err != nil {
			// A check that was attempted and could not be completed is reported
			// rather than passing for one that reached a conclusion. Without
			// the note, a review in which nothing could be checked reads
			// exactly like one in which nothing needed checking.
			v.logf("no evidence for %s: %v", dep.Name, err)
			options.Notes = append(options.Notes, uncheckedNote(candidate.Comment, dep, err))
			continue
		}

		request := verifyRequest{
			Finding:           candidate.Comment.Content,
			DependencyName:    dep.Name,
			DependencyVersion: dep.Version,
			Surface:           surface.Description,
			SurfaceComplete:   surface.Complete,
			Members:           make([]memberEvidence, 0, len(candidate.Members)),
		}
		anyMentioned := false
		for _, member := range candidate.Members {
			lines := surface.Mentions(member)
			if len(lines) > 0 {
				anyMentioned = true
			}
			request.Members = append(request.Members, memberEvidence{Name: member, Lines: lines})
		}
		if !anyMentioned {
			// Nothing in the pinned package mentions any identifier the finding
			// names, so the package does not contradict it. Asking anyway would
			// spend a call to be told the finding stands. On a complete surface
			// that silence is a conclusion; on an incomplete one it may only
			// mean the declaration naming it was never read, and that is an
			// unfinished check worth saying so.
			if !surface.Complete {
				options.Notes = append(options.Notes, uncheckedNote(candidate.Comment, dep, fmt.Errorf(
					"%s could not be read in full and none of `%s` appears in what was read",
					dep.Name, strings.Join(candidate.Members, "`, `"))))
			}
			continue
		}

		result, err := v.judge.Judge(ctx, request)
		if err != nil {
			// The finding is kept and said to be unexamined, because a reader
			// who knows verification ran should not mistake silence for a pass.
			v.logf("could not check %s against %s@%s: %v",
				commentLocation(candidate.Comment), dep.Name, dep.Version, err)
			options.Notes = append(options.Notes, uncheckedNote(candidate.Comment, dep, err))
			continue
		}
		if !dropJustified(result, request.Members) {
			continue
		}

		dropped[findingMarker(candidate.Comment)] = true
		options.Dropped++
		v.logf("dropped a finding on %s: contradicted by %s@%s: %s",
			commentLocation(candidate.Comment), dep.Name, dep.Version, result.Reason)
	}

	if len(dropped) > 0 {
		kept := make([]ocrComment, 0, len(report.Comments))
		for _, comment := range report.Comments {
			if !dropped[findingMarker(comment)] {
				kept = append(kept, comment)
			}
		}
		report.Comments = kept
	}
	return options
}

// uncheckedNote is the summary line for a finding whose check could not be
// finished. Every one of them names the finding and the package it was about,
// so a reader can tell an unfinished check from one that concluded the finding
// stands.
func uncheckedNote(comment ocrComment, dep dependency, cause error) string {
	return fmt.Sprintf("`%s`: not checked against %s@%s (%v)",
		commentLocation(comment), dep.Name, dep.Version, cause)
}

// surfaceFor fetches a dependency's surface once, however many findings name it.
func (v *dependencyVerifier) surfaceFor(ctx context.Context, dep dependency, cache map[string]*apiSurface) (*apiSurface, error) {
	key := dep.Ecosystem + ":" + dep.Name + "@" + dep.Version
	if cached, ok := cache[key]; ok {
		if cached == nil {
			return nil, fmt.Errorf("%s@%s was already looked up and could not be read", dep.Name, dep.Version)
		}
		return cached, nil
	}
	surface, err := v.evidence.Surface(ctx, dep)
	if err != nil {
		cache[key] = nil
		return nil, err
	}
	cache[key] = surface
	return surface, nil
}

// dropJustified is the rule that decides whether a verdict may remove a finding.
//
// It is deliberately harsher than the model's own judgement. A verdict has to
// say the finding is contradicted, give a reason, and name one of the
// identifiers that the evidence actually found -- so a model that answers
// "contradicted" while reasoning about something else, or that answers without
// citing anything, cannot remove a finding by itself. The asymmetry is
// intentional: a false finding that survives costs a reader a minute, and a
// true finding that is dropped hides a defect.
func dropJustified(result verdict, members []memberEvidence) bool {
	if !result.Contradicted || strings.TrimSpace(result.Reason) == "" {
		return false
	}
	for _, member := range members {
		if len(member.Lines) == 0 {
			continue
		}
		if containsWordFold(result.Reason, member.Name) {
			return true
		}
	}
	return false
}
