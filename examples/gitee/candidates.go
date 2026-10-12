// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"path"
	"sort"
	"strings"
)

// candidate is one finding whose claim rests on the API of a pinned dependency.
//
// The shape being targeted is narrow and was measured, not imagined: ocr's own
// filter already removes findings the diff contradicts, and what survives it is
// a claim about a package the diff does not contain. The real example is a
// finding reading "Next.js App Router passes `error` and `reset` to `error.tsx`;
// there is no `retry` prop" -- correct for the version it was thinking of,
// wrong for the one the repository pins.
type candidate struct {
	// Comment is the finding itself.
	Comment ocrComment
	// Dependency is the package the claim is about.
	Dependency dependency
	// Members are the identifiers from that package the finding names.
	Members []string
}

// selectCandidates picks the findings whose claim the pinned dependency could
// settle.
//
// Both halves are required and both are narrow. The finding has to name exactly
// one dependency -- a finding that names two is ambiguous about which package
// its claim is against, and guessing would fetch the wrong evidence. And it has
// to name an API member, which in a code review means an identifier in
// backticks. A finding that merely mentions "React" while discussing the diff's
// own component is not a claim about React's API and is left alone.
func selectCandidates(comments []ocrComment, deps map[string]dependency) []candidate {
	var out []candidate
	for _, comment := range comments {
		mentioned := dependenciesNamed(comment.Content, deps)
		if len(mentioned) != 1 {
			continue
		}
		dep := mentioned[0]
		// A declared range says what the project would accept, not what it
		// uses, so there is no version whose API could be checked.
		if !dep.Exact() {
			continue
		}
		members := membersNamed(comment.Content, dep)
		if len(members) == 0 {
			continue
		}
		out = append(out, candidate{Comment: comment, Dependency: dep, Members: members})
	}
	return out
}

// dependenciesNamed returns the dependencies a finding refers to, in a stable
// order so that a test and a log line agree.
func dependenciesNamed(text string, deps map[string]dependency) []dependency {
	var found []dependency
	for name, dep := range deps {
		if dependencyNamed(text, name) {
			dep.Name = name
			found = append(found, dep)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Name < found[j].Name })
	return found
}

// dependencyNamed reports whether a finding refers to a package by name.
//
// Prose spells a project the way its own documentation does -- "Next.js" for
// next, "React" for react -- so the dotted and capitalised spellings are matched
// anywhere, while the bare name is only accepted inside backticks. Matching the
// bare lowercase name anywhere would fire on ordinary English, and "the next
// step" is not a reference to next.
func dependencyNamed(text, name string) bool {
	normalized := normalizePackageToken(name)
	if normalized == "" {
		return false
	}
	// A module path or a scoped name is never ordinary English, so the full
	// spelling is safe to match anywhere in the text.
	if strings.Contains(normalized, "/") {
		if containsWordFold(text, normalized) {
			return true
		}
		// What follows the host is how a module path is actually written --
		// "github.com/foo/bar" in prose is "foo/bar" -- and that tail is still not
		// ordinary English. A scoped npm name's tail is, though: "@babel/core"
		// would match every finding that happens to say "core", which fetches the
		// evidence for a package the finding is not about. For those the full
		// spelling above and the backticked spans below are the only accepted
		// forms.
		if !strings.HasPrefix(normalized, "@") {
			if index := strings.IndexByte(normalized, '/'); index >= 0 && containsWordFold(text, normalized[index+1:]) {
				return true
			}
		}
	}
	for _, token := range quotedTokens(text) {
		if tokenNamesPackage(token, normalized) {
			return true
		}
	}
	for _, suffix := range []string{".js", ".ts", ".py"} {
		if containsWordFold(text, name+suffix) {
			return true
		}
	}
	if strings.HasPrefix(normalized, "@") {
		return false
	}
	return containsWord(text, titleCase(path.Base(normalized)))
}

// tokenNamesPackage reports whether a backticked span names a package, either
// in full or by the last segment of its path.
func tokenNamesPackage(token, normalizedName string) bool {
	candidate := normalizePackageToken(token)
	if candidate == "" {
		return false
	}
	if candidate == normalizedName {
		return true
	}
	if index := strings.LastIndexByte(normalizedName, '/'); index >= 0 {
		return candidate == normalizedName[index+1:]
	}
	return false
}

// membersNamed returns the identifiers a finding talks about that belong to a
// dependency's API rather than to the repository's own code.
func membersNamed(text string, dep dependency) []string {
	normalized := normalizePackageToken(dep.Name)
	seen := map[string]bool{}
	var members []string
	for _, token := range quotedTokens(text) {
		member := identifierToken(token)
		if member == "" || seen[member] {
			continue
		}
		// The package's own name is not a member of its API, and a finding that
		// only says "next" gives the evidence nothing to look up.
		if strings.EqualFold(member, normalized) {
			continue
		}
		seen[member] = true
		members = append(members, member)
	}
	return members
}

// quotedTokens returns the spans between backticks, which is how a code review
// writes an identifier. An unterminated final span is dropped rather than
// guessed at.
func quotedTokens(text string) []string {
	var tokens []string
	rest := text
	for {
		open := strings.IndexByte(rest, '`')
		if open < 0 {
			return tokens
		}
		rest = rest[open+1:]
		close := strings.IndexByte(rest, '`')
		if close < 0 {
			return tokens
		}
		tokens = append(tokens, rest[:close])
		rest = rest[close+1:]
	}
}

// identifierToken accepts a backticked span that is a single identifier and
// rejects everything else: a path, a filename, a call with arguments, a type
// expression. Being strict here is what keeps a claim about `next.config.js`
// from being read as a claim about a member called "js".
func identifierToken(token string) string {
	candidate := strings.TrimSpace(token)
	candidate = strings.TrimSuffix(candidate, "()")
	if len(candidate) < 2 || len(candidate) > 40 {
		return ""
	}
	for index, r := range candidate {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_', r == '$':
		case r >= '0' && r <= '9' && index > 0:
		default:
			return ""
		}
	}
	if genericIdentifiers[strings.ToLower(candidate)] {
		return ""
	}
	return candidate
}

// genericIdentifiers are the names that appear in any API surface and identify
// nothing: language keywords, primitives, and the handful of words that are the
// same in every package. Looking them up would return pages of declarations
// that settle nothing.
var genericIdentifiers = map[string]bool{
	"any": true, "async": true, "await": true, "boolean": true, "break": true,
	"case": true, "catch": true, "class": true, "const": true, "continue": true,
	"default": true, "delete": true, "else": true, "enum": true, "export": true,
	"extends": true, "false": true, "finally": true, "for": true, "function": true,
	"if": true, "implements": true, "import": true, "in": true, "instanceof": true,
	"interface": true, "let": true, "never": true, "new": true, "null": true,
	"number": true, "object": true, "package": true, "private": true,
	"protected": true, "public": true, "return": true, "static": true,
	"string": true, "super": true, "switch": true, "this": true, "throw": true,
	"true": true, "try": true, "type": true, "typeof": true, "undefined": true,
	"unknown": true, "var": true, "void": true, "while": true, "yield": true,
}

// containsWord reports whether word appears as a whole word, case-sensitively.
func containsWord(text, word string) bool {
	return indexWord(text, word, false) >= 0
}

// containsWordFold is containsWord, ignoring case.
func containsWordFold(text, word string) bool {
	return indexWord(text, word, true) >= 0
}

// indexWord finds word at word boundaries. A hand-rolled scan rather than a
// regexp because it runs for every dependency against every finding, and
// compiling a pattern per pair would cost more than the search.
func indexWord(text, word string, fold bool) int {
	if word == "" || len(word) > len(text) {
		return -1
	}
	haystack := text
	needle := word
	if fold {
		haystack = strings.ToLower(text)
		needle = strings.ToLower(word)
	}
	for offset := 0; ; {
		index := strings.Index(haystack[offset:], needle)
		if index < 0 {
			return -1
		}
		start := offset + index
		end := start + len(needle)
		if !isWordByte(haystack, start-1) && !isWordByte(haystack, end) {
			return start
		}
		offset = start + 1
	}
}

// isWordByte reports whether the byte at index continues a word. An index
// outside the string is a boundary.
func isWordByte(text string, index int) bool {
	if index < 0 || index >= len(text) {
		return false
	}
	c := text[index]
	return c == '_' || c == '$' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// titleCase capitalises the first letter, which is how a project writes its own
// name in prose.
func titleCase(word string) string {
	if word == "" {
		return ""
	}
	return strings.ToUpper(word[:1]) + word[1:]
}
