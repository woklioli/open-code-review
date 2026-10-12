// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"errors"
	"testing"
)

// fakeFiles stands in for the reviewed revision's checkout.
type fakeFiles map[string]string

func (f fakeFiles) ReadFile(name string) ([]byte, error) {
	if content, ok := f[name]; ok {
		return []byte(content), nil
	}
	return nil, errors.New("no such file: " + name)
}

func TestParsePackageJSON(t *testing.T) {
	deps := parsePackageJSON([]byte(`{
		"dependencies": {"next": "^16.3.0", "react": "19.0.0"},
		"devDependencies": {"typescript": "~5.6.0"},
		"peerDependencies": {"react-dom": "^19.0.0"},
		"optionalDependencies": {"fsevents": "^2.3.0"}
	}`))
	for name, wantRange := range map[string]string{
		"next": "^16.3.0", "react": "19.0.0", "typescript": "~5.6.0",
		"react-dom": "^19.0.0", "fsevents": "^2.3.0",
	} {
		dep, ok := deps[name]
		if !ok {
			t.Errorf("%s missing from %v", name, deps)
			continue
		}
		if dep.Range != wantRange {
			t.Errorf("%s range = %q, want %q", name, dep.Range, wantRange)
		}
		if dep.Exact() {
			t.Errorf("%s: a declared range is not an exact version", name)
		}
	}
}

func TestParsePackageJSONRejectsMalformedInput(t *testing.T) {
	if got := parsePackageJSON([]byte(`{"dependencies": `)); got != nil {
		t.Errorf("got %v", got)
	}
}

// npm 7 and later key the tree by install path, and the same package can appear
// under several of them; npm 6 used a flat dependencies map.
func TestParsePackageLock(t *testing.T) {
	deps := parsePackageLock([]byte(`{
		"packages": {
			"": {"name": "the-project", "version": "1.0.0"},
			"node_modules/next": {"version": "16.3.0"},
			"node_modules/a/node_modules/next": {"version": "15.0.0"},
			"node_modules/@scope/thing": {"version": "2.1.0"}
		}
	}`))
	if deps["next"].Version != "15.0.0" && deps["next"].Version != "16.3.0" {
		t.Errorf("next = %+v", deps["next"])
	}
	if got := deps["@scope/thing"].Version; got != "2.1.0" {
		t.Errorf("a scoped name must survive the install path, got %q", got)
	}
	if _, ok := deps["the-project"]; ok {
		t.Error("the project's own entry has no install path and is not a dependency")
	}

	v1 := parsePackageLock([]byte(`{"dependencies": {"lodash": {"version": "4.17.21"}}}`))
	if got := v1["lodash"].Version; got != "4.17.21" {
		t.Errorf("npm 6 shape = %+v", v1["lodash"])
	}
}

func TestPackageNameFromInstallPath(t *testing.T) {
	cases := map[string]string{
		"node_modules/next":                    "next",
		"node_modules/a/node_modules/next":     "next",
		"node_modules/@scope/thing":            "@scope/thing",
		"node_modules/a/node_modules/@s/thing": "@s/thing",
		"":                                     "",
		"src/local.ts":                         "",
	}
	for input, want := range cases {
		if got := packageNameFromInstallPath(input); got != want {
			t.Errorf("packageNameFromInstallPath(%q) = %q, want %q", input, got, want)
		}
	}
}

// pnpm has spelled a package three ways; a scoped name contains both a slash
// and an at-sign, so a wrong split files the version as part of the name.
func TestParsePNPMLock(t *testing.T) {
	got := parsePNPMLock([]byte(`
lockfileVersion: '6.0'

packages:

  /lodash@4.17.21:
    resolution: {integrity: sha512-...}

  /@babel/core@7.22.5:
    resolution: {integrity: sha512-...}

  /@babel/core@7.22.5(esbuild@0.19.0):
    resolution: {integrity: sha512-...}
`))
	for name, want := range map[string]string{
		"lodash":      "4.17.21",
		"@babel/core": "7.22.5",
	} {
		if got[name].Version != want {
			t.Errorf("%s = %+v, want version %s", name, got[name], want)
		}
	}

	v5 := parsePNPMLock([]byte("\n  /lodash/4.17.21:\n    resolution: {}\n"))
	if v5["lodash"].Version != "4.17.21" {
		t.Errorf("the v5 spelling = %+v", v5["lodash"])
	}
	v9 := parsePNPMLock([]byte("\n  '@babel/core@7.22.5':\n    resolution: {}\n"))
	if v9["@babel/core"].Version != "7.22.5" {
		t.Errorf("the v9 spelling = %+v", v9["@babel/core"])
	}
}

func TestParseYarnLock(t *testing.T) {
	got := parseYarnLock([]byte(`
"@babel/core@^7.0.0", "@babel/core@^7.1.0":
  version "7.22.5"
  resolved "https://registry.yarnpkg.com/..."

lodash@^4.17.0:
  version "4.17.21"
  resolved "https://registry.yarnpkg.com/..."
`))
	if got["@babel/core"].Version != "7.22.5" {
		t.Errorf("a scoped name = %+v", got["@babel/core"])
	}
	if got["lodash"].Version != "4.17.21" {
		t.Errorf("lodash = %+v", got["lodash"])
	}
}

func TestParseGoMod(t *testing.T) {
	got := parseGoMod([]byte(`module github.com/me/project

go 1.26

require (
	github.com/spf13/cobra v1.8.1
	golang.org/x/net v0.30.0 // indirect
)

require github.com/stretchr/testify v1.9.0
`))
	for name, want := range map[string]string{
		"github.com/spf13/cobra":      "v1.8.1",
		"golang.org/x/net":            "v0.30.0",
		"github.com/stretchr/testify": "v1.9.0",
	} {
		if got[name].Version != want {
			t.Errorf("%s = %+v, want %s", name, got[name], want)
		}
	}
}

// Only a pin names a version, and a version is the only thing that can be
// checked against a published package.
func TestParseRequirementsTxt(t *testing.T) {
	got := parseRequirementsTxt([]byte(`
# comment
flask==3.0.0
requests>=2.31.0
django == 5.0.2  # pinned
-r other.txt
`))
	if got["flask"].Version != "3.0.0" {
		t.Errorf("flask = %+v", got["flask"])
	}
	if got["django"].Version != "5.0.2" {
		t.Errorf("django = %+v", got["django"])
	}
	if _, ok := got["requests"]; ok {
		t.Error("an unpinned requirement names no version and is not evidence")
	}
}

func TestLoadDependenciesPrefersAnExactVersion(t *testing.T) {
	// The lock file is listed first, but the point is that an exact version wins
	// wherever it comes from.
	deps := loadDependencies(context.Background(), fakeFiles{
		"package.json":      `{"dependencies": {"next": "^16.3.0"}}`,
		"package-lock.json": `{"packages": {"node_modules/next": {"version": "16.3.7"}}}`,
		"go.mod":            "module x\n\nrequire github.com/spf13/cobra v1.8.1\n",
	})
	if got := deps["next"].Version; got != "16.3.7" {
		t.Errorf("the lock file's exact version should win, got %q", got)
	}
	if got := deps["next"].Ecosystem; got != "npm" {
		t.Errorf("ecosystem = %q", got)
	}
	if got := deps["github.com/spf13/cobra"].Ecosystem; got != "go" {
		t.Errorf("ecosystem = %q", got)
	}
}

func TestLoadDependenciesFallsBackToTheRange(t *testing.T) {
	deps := loadDependencies(context.Background(), fakeFiles{
		"package.json": `{"dependencies": {"next": "^16.3.0"}}`,
	})
	if dep := deps["next"]; dep.Exact() || dep.Range != "^16.3.0" {
		t.Errorf("got %+v", dep)
	}
}

// A malformed or missing manifest costs candidates, and a missing candidate
// only means a finding is kept.
func TestLoadDependenciesToleratesUnreadableManifests(t *testing.T) {
	deps := loadDependencies(context.Background(), fakeFiles{
		"package.json": `{"dependencies": `,
		"go.mod":       "module x\n\nrequire github.com/spf13/cobra v1.8.1\n",
	})
	if _, ok := deps["next"]; ok {
		t.Error("a malformed manifest should contribute nothing")
	}
	if deps["github.com/spf13/cobra"].Version != "v1.8.1" {
		t.Error("one bad manifest must not stop the others being read")
	}
}

func TestLoadDependenciesWithNothingPresent(t *testing.T) {
	if got := loadDependencies(context.Background(), fakeFiles{}); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestLoadDependenciesStopsOnACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := loadDependencies(ctx, fakeFiles{"go.mod": "require x/y v1.0.0"}); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestNormalizePackageToken(t *testing.T) {
	cases := map[string]string{
		"Next.js": "next", "`next`": "next", "React": "react",
		"next()": "next", "flask.py": "flask", "  Lodash  ": "lodash",
		"@scope/Thing": "@scope/thing", ".js": ".js",
	}
	for input, want := range cases {
		if got := normalizePackageToken(input); got != want {
			t.Errorf("normalizePackageToken(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestIsVersionLike(t *testing.T) {
	for input, want := range map[string]bool{
		"1.2.3": true, "4": true, "": false, "lodash": false, "v1.2.3": false,
	} {
		if got := isVersionLike(input); got != want {
			t.Errorf("isVersionLike(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestSplitPackageSpec(t *testing.T) {
	cases := map[string][2]string{
		"lodash@4.17.21":     {"lodash", "4.17.21"},
		"@babel/core@7.22.5": {"@babel/core", "7.22.5"},
		"lodash/4.17.21":     {"lodash", "4.17.21"},
		"@babel/core/7.22.5": {"@babel/core", "7.22.5"},
		"lodash":             {"", ""},
	}
	for input, want := range cases {
		name, version := splitPackageSpec(input)
		if name != want[0] || version != want[1] {
			t.Errorf("splitPackageSpec(%q) = %q, %q, want %q, %q", input, name, version, want[0], want[1])
		}
	}
}
