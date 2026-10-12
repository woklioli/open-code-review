// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// dependency is a third-party package the reviewed revision pins.
type dependency struct {
	// Name is spelled the way the manifest spells it, which is how a comment
	// would name it too.
	Name string
	// Version is the exact published version. It comes from a lock file and is
	// what makes the evidence meaningful: "next has no retry prop" is a claim
	// about a version, not about a package.
	Version string
	// Range is the declared constraint, used when no lock file pins a version.
	// A range is not evidence of anything on its own.
	Range string
	// Ecosystem decides which surface can be fetched for it.
	Ecosystem string
}

// Exact reports whether the version is pinned rather than a range.
func (d dependency) Exact() bool { return d.Version != "" }

// fileSource reads a file out of the reviewed revision.
type fileSource interface {
	ReadFile(name string) ([]byte, error)
}

// manifestEntry is one manifest this service knows how to read. Order matters:
// a lock file pins an exact version, so it has to be read before the manifest
// whose ranges it resolves.
type manifestEntry struct {
	path      string
	ecosystem string
	parse     func([]byte) map[string]dependency
}

var manifestEntries = []manifestEntry{
	{"package-lock.json", "npm", parsePackageLock},
	{"pnpm-lock.yaml", "npm", parsePNPMLock},
	{"yarn.lock", "npm", parseYarnLock},
	{"package.json", "npm", parsePackageJSON},
	{"go.mod", "go", parseGoMod},
	{"requirements.txt", "pypi", parseRequirementsTxt},
}

// loadDependencies merges every manifest present in the revision.
//
// A manifest that cannot be read is treated as absent rather than as an error.
// Every failure here costs candidates, and a missing candidate only means a
// finding is kept -- which is the direction this whole feature is built to fail
// in. The alternative, refusing to verify anything because one lock file was
// malformed, would be strictly worse.
func loadDependencies(ctx context.Context, source fileSource) map[string]dependency {
	merged := map[string]dependency{}
	for _, entry := range manifestEntries {
		if err := ctx.Err(); err != nil {
			return merged
		}
		data, err := source.ReadFile(entry.path)
		if err != nil {
			continue
		}
		for name, dep := range entry.parse(data) {
			existing, seen := merged[name]
			dep.Ecosystem = entry.ecosystem
			// The first entry that pins an exact version wins, and an exact
			// version always beats a range whichever order they arrived in.
			switch {
			case !seen:
				merged[name] = dep
			case dep.Exact() && !existing.Exact():
				merged[name] = dep
			}
		}
	}
	return merged
}

// parsePackageJSON reads declared dependencies. The values are ranges such as
// "^16.3.0", which say what the project would accept rather than what it uses.
func parsePackageJSON(data []byte) map[string]dependency {
	var manifest struct {
		Dependencies     map[string]string `json:"dependencies"`
		DevDependencies  map[string]string `json:"devDependencies"`
		PeerDependencies map[string]string `json:"peerDependencies"`
		OptionalDeps     map[string]string `json:"optionalDependencies"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil
	}
	out := map[string]dependency{}
	for _, group := range []map[string]string{
		manifest.Dependencies, manifest.DevDependencies,
		manifest.PeerDependencies, manifest.OptionalDeps,
	} {
		for name, want := range group {
			if strings.TrimSpace(name) == "" {
				continue
			}
			if _, seen := out[name]; !seen {
				out[name] = dependency{Name: name, Range: strings.TrimSpace(want)}
			}
		}
	}
	return out
}

// parsePackageLock reads the resolved tree. Both shapes matter: npm 6 wrote a
// nested "dependencies" map, npm 7 and later write "packages" keyed by install
// path.
func parsePackageLock(data []byte) map[string]dependency {
	var lock struct {
		Packages map[string]struct {
			Version string `json:"version"`
			Dev     bool   `json:"dev"`
		} `json:"packages"`
		Dependencies map[string]struct {
			Version string `json:"version"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil
	}
	out := map[string]dependency{}
	// npm keeps a copy of a package at every level that needs a different
	// version, so one name can appear under several install paths. Which copy
	// wins has to be decided by the path and not by Go's randomised map
	// iteration: the version chosen here is the key the evidence is fetched by,
	// and a version that varied between runs would drop a finding on one run and
	// keep it on the next.
	chosen := map[string]string{}
	for installPath, entry := range lock.Packages {
		name := packageNameFromInstallPath(installPath)
		if name == "" || entry.Version == "" {
			continue
		}
		if !shallower(installPath, chosen[name]) {
			continue
		}
		out[name] = dependency{Name: name, Version: entry.Version}
		chosen[name] = installPath
	}
	for name, entry := range lock.Dependencies {
		if entry.Version == "" {
			continue
		}
		if _, seen := out[name]; !seen {
			out[name] = dependency{Name: name, Version: entry.Version}
		}
	}
	return out
}

// shallower reports whether one install path should beat another for the same
// package name. The shortest path is the hoisted copy, which is the one the
// project's own imports resolve to; the path itself breaks a tie so the answer
// does not depend on the order the map happened to be walked in.
func shallower(candidate, current string) bool {
	if current == "" {
		return true
	}
	if len(candidate) != len(current) {
		return len(candidate) < len(current)
	}
	return candidate < current
}

// packageNameFromInstallPath turns "node_modules/a/node_modules/@s/b" into
// "@s/b". The empty key, which npm uses for the project itself, has no name.
func packageNameFromInstallPath(installPath string) string {
	const marker = "node_modules/"
	index := strings.LastIndex(installPath, marker)
	if index < 0 {
		return ""
	}
	name := installPath[index+len(marker):]
	if name == "" {
		return ""
	}
	return name
}

// parsePNPMLock reads pnpm's lockfile, which has spelled a package three ways
// across versions: "/name/1.2.3" (v5), "/name@1.2.3" (v6) and "name@1.2.3"
// (v9). All three are handled, because a scoped name contains both a slash and
// an at-sign and a wrong split would file the version as part of the name.
func parsePNPMLock(data []byte) map[string]dependency {
	out := map[string]dependency{}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasSuffix(trimmed, ":") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key := strings.Trim(strings.TrimSuffix(trimmed, ":"), `"'`)
		key = strings.TrimPrefix(key, "/")
		key = trimPeerSuffix(key)
		name, version := splitPackageSpec(key)
		if name == "" || !isVersionLike(version) {
			continue
		}
		out[name] = dependency{Name: name, Version: version}
	}
	return out
}

// trimPeerSuffix removes the peer-dependency suffix pnpm writes after a version.
// There are two spellings: "7.31.10(eslint@8.25.0)" from pnpm 7 onwards, and
// "7.31.10_eslint@8.25.0" from pnpm 5 and 6. Leaving the older one in place
// makes splitPackageSpec cut at the peer's at-sign, filing the peer into the
// name and the peer's version as the version, so the package itself is left
// unpinned and every finding about it goes unchecked.
func trimPeerSuffix(key string) string {
	if index := strings.IndexByte(key, '('); index >= 0 {
		return key[:index]
	}
	// The underscore form is only a suffix when a peer version follows it: a
	// package name may legitimately contain an underscore, and cutting at the
	// first one would truncate those names.
	if index := strings.IndexByte(key, '_'); index >= 0 && strings.Contains(key[index+1:], "@") {
		return key[:index]
	}
	return key
}

// splitPackageSpec divides "name@version" or "name/version" into its parts. The
// separator is the last one whose right-hand side holds no at-sign, so that a
// scoped name such as "@babel/core" survives intact.
func splitPackageSpec(spec string) (name, version string) {
	if index := strings.LastIndexByte(spec, '/'); index > 0 && !strings.Contains(spec[index+1:], "@") {
		return spec[:index], spec[index+1:]
	}
	if index := strings.LastIndexByte(spec, '@'); index > 0 {
		return spec[:index], spec[index+1:]
	}
	return "", ""
}

// parseYarnLock reads the classic (v1) yarn lockfile, whose entries look like
//
//	"@scope/name@^1.2.3", "@scope/name@^1.3.0":
//	  version "1.2.4"
//
// The block is parsed as text rather than with a regexp because the header
// lists several ranges for one resolution and a scoped name holds the same
// characters the separator does.
func parseYarnLock(data []byte) map[string]dependency {
	out := map[string]dependency{}
	for _, block := range strings.Split(string(data), "\n\n") {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		if len(lines) < 2 {
			continue
		}
		version := ""
		for _, line := range lines[1:] {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "version ") {
				version = strings.Trim(strings.TrimPrefix(trimmed, "version "), `"'`)
				break
			}
		}
		if version == "" {
			continue
		}
		for _, spec := range strings.Split(lines[0], ",") {
			name, _ := splitPackageSpec(strings.Trim(strings.TrimSpace(spec), `"':`))
			if name == "" {
				continue
			}
			out[name] = dependency{Name: name, Version: version}
		}
	}
	return out
}

// parseGoMod reads the module requirements. A Go build list is exact by
// construction: the version written is the one selected, not a range.
var goRequire = regexp.MustCompile(`(?m)^\s*(?:require\s+)?([a-zA-Z0-9._~\-]+(?:\.[a-zA-Z]{2,})?[^\s]*\/[^\s]+)\s+(v[^\s]+)`)

func parseGoMod(data []byte) map[string]dependency {
	out := map[string]dependency{}
	for _, match := range goRequire.FindAllStringSubmatch(string(data), -1) {
		name, version := match[1], strings.TrimSuffix(match[2], "//")
		if strings.Contains(version, "/") {
			// A line such as "require (" matched too greedily; the version is
			// always a single token starting with v.
			continue
		}
		out[name] = dependency{Name: name, Version: version}
	}
	return out
}

// parseRequirementsTxt reads pip requirements. Only pinned entries count: an
// unpinned one says nothing about which version is on the machine.
var requirementsPin = regexp.MustCompile(`(?m)^\s*([A-Za-z0-9][A-Za-z0-9._-]*)\s*==\s*([^\s;#]+)`)

func parseRequirementsTxt(data []byte) map[string]dependency {
	out := map[string]dependency{}
	for _, match := range requirementsPin.FindAllStringSubmatch(string(data), -1) {
		name := strings.TrimSpace(match[1])
		out[name] = dependency{Name: name, Version: strings.TrimSpace(match[2])}
	}
	return out
}

// isVersionLike keeps lockfile keys from being read as versions. A pnpm key
// like "/lodash" has no version at all, and "1.2.3_peer" has been trimmed
// already by the time this is called.
func isVersionLike(candidate string) bool {
	if candidate == "" {
		return false
	}
	return candidate[0] >= '0' && candidate[0] <= '9'
}

// normalizePackageToken lowercases a token and strips the file suffixes prose
// adds to a package name.
func normalizePackageToken(token string) string {
	trimmed := strings.TrimSpace(token)
	trimmed = strings.TrimPrefix(trimmed, "`")
	trimmed = strings.TrimSuffix(trimmed, "`")
	trimmed = strings.TrimSuffix(trimmed, "()")
	for _, suffix := range []string{".js", ".ts", ".py", ".rb", ".go"} {
		if len(trimmed) > len(suffix) && strings.HasSuffix(strings.ToLower(trimmed), suffix) {
			trimmed = trimmed[:len(trimmed)-len(suffix)]
			break
		}
	}
	return strings.ToLower(strings.TrimSpace(trimmed))
}
