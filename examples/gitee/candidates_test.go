// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"strings"
	"testing"
)

// retryPropFinding is one of the two false positives measured on a real merge
// request, copied verbatim. ocr's own filter keeps it by design -- the diff
// contains no Next.js source to contradict it -- so it is the case this whole
// round exists for, and the fixture the selector is calibrated against.
const retryPropFinding = "Next.js App Router passes `error` and `reset` to `error.tsx`; there is no `retry` prop. " +
	"Destructuring `retry` yields `undefined`, so `FrontendErrorFallback`'s retry button will be a no-op. " +
	"Rename to `reset` (or alias `reset: retry`)."

func nextDependency() map[string]dependency {
	return map[string]dependency{"next": {Name: "next", Version: "16.3.0", Ecosystem: "npm"}}
}

func TestSelectCandidatesFindsTheMeasuredFalsePositive(t *testing.T) {
	candidates := selectCandidates([]ocrComment{{Path: "app/error.tsx", Content: retryPropFinding}}, nextDependency())
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want 1", len(candidates))
	}
	got := candidates[0]
	if got.Dependency.Name != "next" || got.Dependency.Version != "16.3.0" {
		t.Errorf("dependency = %+v", got.Dependency)
	}
	// The members are the identifiers the evidence has to be looked up for, so
	// the decisive one has to be among them.
	if !contains(got.Members, "retry") {
		t.Errorf("members = %v, want `retry` among them", got.Members)
	}
	// A filename and a language primitive are not API members, and looking them
	// up would only add noise to the evidence.
	for _, unwanted := range []string{"error.tsx", "undefined"} {
		if contains(got.Members, unwanted) {
			t.Errorf("%q is not an API member, members = %v", unwanted, got.Members)
		}
	}
}

func TestSelectCandidatesSkipsFindingsAboutTheDiffItself(t *testing.T) {
	cases := map[string]string{
		"no dependency named":   "This `retry` helper swallows the error and the caller never learns the read failed.",
		"no member named":       "Next.js is used here, and the component re-renders on every keystroke.",
		"member is the package": "Next.js should be upgraded before this pattern is relied on.",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if got := selectCandidates([]ocrComment{{Content: content}}, nextDependency()); len(got) != 0 {
				t.Errorf("got %d candidates: %+v", len(got), got)
			}
		})
	}
}

// Two packages named in one finding make the claim's target ambiguous, and
// fetching the wrong package's surface would be worse than not checking.
func TestSelectCandidatesRefusesAmbiguousFindings(t *testing.T) {
	deps := map[string]dependency{
		"next":  {Name: "next", Version: "16.3.0", Ecosystem: "npm"},
		"react": {Name: "react", Version: "19.0.0", Ecosystem: "npm"},
	}
	content := "Next.js and React both ignore the `retry` prop here."
	if got := selectCandidates([]ocrComment{{Content: content}}, deps); len(got) != 0 {
		t.Errorf("got %d candidates: %+v", len(got), got)
	}
}

// A range says what the project would accept, so there is no version whose API
// could be checked.
func TestSelectCandidatesNeedsAnExactVersion(t *testing.T) {
	deps := map[string]dependency{"next": {Name: "next", Range: "^16.3.0", Ecosystem: "npm"}}
	if got := selectCandidates([]ocrComment{{Content: retryPropFinding}}, deps); len(got) != 0 {
		t.Errorf("got %d candidates: %+v", len(got), got)
	}
}

func TestSelectCandidatesWithNoDependencies(t *testing.T) {
	if got := selectCandidates([]ocrComment{{Content: retryPropFinding}}, nil); len(got) != 0 {
		t.Errorf("got %d candidates", len(got))
	}
}

func TestQuotedTokens(t *testing.T) {
	cases := map[string][]string{
		"plain `a` and `b` here": {"a", "b"},
		"no backticks at all":    nil,
		"unterminated `dangling": nil,
		"``":                     {""},
		"`a` then `unterminated": {"a"},
		"text before `only`":     {"only"},
	}
	for input, want := range cases {
		got := quotedTokens(input)
		if len(got) != len(want) {
			t.Errorf("quotedTokens(%q) = %v, want %v", input, got, want)
			continue
		}
		for index := range want {
			if got[index] != want[index] {
				t.Errorf("quotedTokens(%q) = %v, want %v", input, got, want)
				break
			}
		}
	}
}

func TestIdentifierToken(t *testing.T) {
	accepted := map[string]string{
		"retry": "retry", "reset": "reset", "useMemo": "useMemo",
		"$scope": "$scope", "_private": "_private", "a1": "a1",
		"call()": "call", "  padded  ": "padded", "ReactNode": "ReactNode",
	}
	for input, want := range accepted {
		if got := identifierToken(input); got != want {
			t.Errorf("identifierToken(%q) = %q, want %q", input, got, want)
		}
	}
	// Every rejection here is something the evidence lookup would gain nothing
	// from and could be misled by.
	rejected := []string{
		"error.tsx", "next.config.js", "src/utils.ts", "a.b", "foo bar",
		"a-b", "1abc", "x", strings.Repeat("a", 41), "", "a()b",
		"undefined", "string", "true", "function", "return", "yield",
	}
	for _, input := range rejected {
		if got := identifierToken(input); got != "" {
			t.Errorf("identifierToken(%q) = %q, want rejection", input, got)
		}
	}
}

func TestDependencyNamed(t *testing.T) {
	cases := map[string]struct {
		text string
		want bool
	}{
		"prose project name": {"Next.js App Router passes `error`.", true},
		"dotted lowercase":   {"next.js misbehaves here", true},
		"backticked":         {"the `next` package is pinned", true},
		"ordinary english":   {"The next step is to validate the input before use.", false},
		"next in a sentence": {"then the next handler runs", false},
		"unrelated":          {"this has nothing to do with the package", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := dependencyNamed(tc.text, "next"); got != tc.want {
				t.Errorf("dependencyNamed(%q, next) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

func TestDependencyNamedForAModulePath(t *testing.T) {
	const module = "github.com/gorilla/mux"
	// A module path is never ordinary English, so the way prose writes it and
	// the way a review writes it in backticks are both accepted.
	for _, text := range []string{
		"This uses gorilla/mux for routing.",
		"This uses `gorilla/mux` for routing.",
		"This uses `mux` for routing.",
		"This uses github.com/gorilla/mux for routing.",
	} {
		if !dependencyNamed(text, module) {
			t.Errorf("%q should name the module", text)
		}
	}
	// The bare last segment in prose is too weak: "mux" is a word in this
	// sentence, not a reference to a package.
	if dependencyNamed("this muxes the streams together", module) {
		t.Error("a bare lowercase last segment should not match")
	}
}

// A scoped name's last segment is the bare package name, which is ordinary
// English. Matching it would fetch the wrong package's evidence and, worse, let
// that package's own name count as a mention of the identifier the finding
// names -- so a finding about the repository's own code would be judged against
// an unrelated package.
func TestDependencyNamedForAScopedPackage(t *testing.T) {
	const scoped = "@babel/core"
	for _, text := range []string{
		"This uses @babel/core to transform the file.",
		"This uses `@babel/core` to transform the file.",
		"This uses `@babel/core` for the transform.",
	} {
		if !dependencyNamed(text, scoped) {
			t.Errorf("%q should name the scoped package", text)
		}
	}
	for _, text := range []string{
		"The core loop leaks memory under load.",
		"Wrap the whole thing in a Core abstraction.",
	} {
		if dependencyNamed(text, scoped) {
			t.Errorf("%q does not name %s", text, scoped)
		}
	}
}

// The module-path tail rule is unchanged: what follows the host is still not
// ordinary English there.
func TestDependencyNamedModulePathTailStillMatches(t *testing.T) {
	if !dependencyNamed("This uses gorilla/mux for routing.", "github.com/gorilla/mux") {
		t.Error("a module path tail should still match")
	}
}

func TestMembersNamedDeduplicatesAndKeepsOrder(t *testing.T) {
	deps := nextDependency()["next"]
	members := membersNamed(retryPropFinding, deps)
	if got := strings.Join(members, ","); got != "error,reset,retry,FrontendErrorFallback" {
		t.Errorf("members = %q", got)
	}
}

func TestContainsWord(t *testing.T) {
	cases := map[string]struct {
		text, word string
		want       bool
	}{
		"whole word":        {"the next step", "next", true},
		"not a whole word":  {"nextjs is different", "next", false},
		"at the start":      {"next thing", "next", true},
		"at the end":        {"the next", "next", true},
		"underscore joins":  {"my_next_thing", "next", false},
		"case differs":      {"the Next step", "next", false},
		"folded case":       {"the Next step", "next", true},
		"dot is a boundary": {"Next.js", "Next", true},
		"empty needle":      {"anything", "", false},
		"needle longer":     {"ab", "abcdef", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var got bool
			if strings.Contains(name, "folded") {
				got = containsWordFold(tc.text, tc.word)
			} else {
				got = containsWord(tc.text, tc.word)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTitleCase(t *testing.T) {
	if got := titleCase("next"); got != "Next" {
		t.Errorf("got %q", got)
	}
	if got := titleCase(""); got != "" {
		t.Errorf("got %q", got)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
