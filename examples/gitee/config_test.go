// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// baseEnv returns the minimum set of required variables, so each test can
// vary exactly one thing and attribute a failure to it.
func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GITEE_TOKEN", "token-value")
	t.Setenv("GITEE_WEBHOOK_SECRET", "secret-value")
	t.Setenv("OCR_LLM_URL", "https://llm.example/v1")
	t.Setenv("OCR_LLM_TOKEN", "llm-token")
	t.Setenv("OCR_LLM_MODEL", "some-model")
}

func writeEnvFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	return path
}

func TestParseEnvLine(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		wantKey   string
		wantValue string
		wantOK    bool
	}{
		{"plain", "KEY=value", "KEY", "value", true},
		{"spaces around", "  KEY = value  ", "KEY", "value", true},
		{"export prefix", "export KEY=value", "KEY", "value", true},
		{"blank", "   ", "", "", false},
		{"comment", "# a note", "", "", false},
		{"no equals", "KEY", "", "", false},
		{"empty key", "=value", "", "", false},
		{"empty value", "KEY=", "KEY", "", true},
		{"double quoted keeps hash", `KEY="a # b"`, "KEY", "a # b", true},
		{"single quoted keeps spaces", "KEY=' padded '", "KEY", " padded ", true},
		{"unquoted trailing comment", "KEY=value # why", "KEY", "value", true},
		{"hash inside value is data", "KEY=a#b", "KEY", "a#b", true},
		{"opening quote without a match", `KEY="a"b`, "KEY", `"a"b`, true},
		// An unterminated quote is dropped rather than returned with the quote
		// still attached: a credential carrying a stray quote fails every request
		// with a 401 that says nothing about the cause, while a dropped line is
		// reported as a missing variable, which names it.
		{"unterminated double quote", `KEY="abc`, "", "", false},
		{"unterminated single quote", `KEY='abc`, "", "", false},
		{"lone quote is not a value", `KEY="`, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, value, ok := parseEnvLine(tc.line)
			if ok != tc.wantOK || key != tc.wantKey || value != tc.wantValue {
				t.Fatalf("parseEnvLine(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.line, key, value, ok, tc.wantKey, tc.wantValue, tc.wantOK)
			}
		})
	}
}

func TestReadEnvFile(t *testing.T) {
	t.Run("missing file is not an error", func(t *testing.T) {
		vars, err := readEnvFile(filepath.Join(t.TempDir(), "absent.env"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(vars) != 0 {
			t.Fatalf("want empty map, got %v", vars)
		}
	})

	t.Run("empty path is not an error", func(t *testing.T) {
		vars, err := readEnvFile("")
		if err != nil || len(vars) != 0 {
			t.Fatalf("readEnvFile(\"\") = (%v, %v), want empty and nil", vars, err)
		}
	})

	t.Run("parses values and skips noise", func(t *testing.T) {
		path := writeEnvFile(t, "# leading comment\nA=1\n\nexport B=two\nC=three # tail\n")
		vars, err := readEnvFile(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := map[string]string{"A": "1", "B": "two", "C": "three"}
		if len(vars) != len(want) {
			t.Fatalf("got %v, want %v", vars, want)
		}
		for k, v := range want {
			if vars[k] != v {
				t.Errorf("vars[%q] = %q, want %q", k, vars[k], v)
			}
		}
	})

	// A repeated key is ambiguous in a credential file: the operator almost
	// certainly edited one line and left the other, and silently preferring
	// either would hide the mistake.
	t.Run("duplicate key is an error", func(t *testing.T) {
		path := writeEnvFile(t, "A=1\nA=2\n")
		if _, err := readEnvFile(path); err == nil {
			t.Fatal("want an error for a duplicate key")
		} else if !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("error should name the problem, got: %v", err)
		}
	})

	t.Run("unreadable path is an error", func(t *testing.T) {
		if _, err := readEnvFile(t.TempDir()); err == nil {
			t.Fatal("want an error when the path is a directory")
		}
	})
}

func TestLoadConfigDefaults(t *testing.T) {
	baseEnv(t)
	for _, key := range []string{
		"GITEE_API_BASE", "GITEE_ADDR", "OCR_BIN", "GITEE_WORK_ROOT",
		"GITEE_VERIFY_DEPS", "GITEE_DRY_RUN", "GITEE_REVIEW_TIMEOUT_MIN", "GITEE_CONCURRENCY",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GiteeAPIBase != defaultGiteeAPIBase {
		t.Errorf("GiteeAPIBase = %q, want %q", cfg.GiteeAPIBase, defaultGiteeAPIBase)
	}
	if cfg.Addr != defaultAddr {
		t.Errorf("Addr = %q, want %q", cfg.Addr, defaultAddr)
	}
	if cfg.OcrBin != defaultOcrBin {
		t.Errorf("OcrBin = %q, want %q", cfg.OcrBin, defaultOcrBin)
	}
	if cfg.ReviewTimeoutMin != defaultReviewTimeout {
		t.Errorf("ReviewTimeoutMin = %d, want %d", cfg.ReviewTimeoutMin, defaultReviewTimeout)
	}
	if cfg.Concurrency != defaultConcurrency {
		t.Errorf("Concurrency = %d, want %d", cfg.Concurrency, defaultConcurrency)
	}
	// The two safe defaults: verify is on, posting is off until asked for.
	if !cfg.VerifyDeps {
		t.Error("VerifyDeps should default to true")
	}
	if !cfg.DryRun {
		t.Error("DryRun should default to true so nothing is posted before it is turned off")
	}
}

func TestLoadConfigRequiredVariables(t *testing.T) {
	cases := []struct {
		name    string
		unset   string
		wantErr string
	}{
		{"token", "GITEE_TOKEN", "GITEE_TOKEN"},
		{"webhook secret", "GITEE_WEBHOOK_SECRET", "GITEE_WEBHOOK_SECRET"},
		{"llm url", "OCR_LLM_URL", "OCR_LLM_URL"},
		{"llm token", "OCR_LLM_TOKEN", "OCR_LLM_TOKEN"},
		{"llm model", "OCR_LLM_MODEL", "OCR_LLM_MODEL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseEnv(t)
			t.Setenv(tc.unset, "")
			os.Unsetenv(tc.unset)
			_, err := LoadConfig("")
			if err == nil {
				t.Fatalf("want an error when %s is unset", tc.unset)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error should name %s, got: %v", tc.wantErr, err)
			}
		})
	}
}

// All missing variables are reported together: fixing them one restart at a
// time is exactly the friction this validation exists to remove.
func TestLoadConfigReportsEveryMissingVariable(t *testing.T) {
	for _, key := range []string{"GITEE_TOKEN", "GITEE_WEBHOOK_SECRET", "OCR_LLM_URL", "OCR_LLM_TOKEN", "OCR_LLM_MODEL"} {
		// Register the variable for restoration first, then remove it: the
		// loader only consults the file for variables that are absent, so
		// leaving them present-but-empty would shadow the file under test.
		t.Setenv(key, "sentinel")
		os.Unsetenv(key)
	}
	_, err := LoadConfig("")
	if err == nil {
		t.Fatal("want an error")
	}
	for _, key := range []string{"GITEE_TOKEN", "GITEE_WEBHOOK_SECRET", "OCR_LLM_URL", "OCR_LLM_TOKEN", "OCR_LLM_MODEL"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error should mention %s, got: %v", key, err)
		}
	}
}

func TestLoadConfigRejectsNonPositiveNumbers(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		baseEnv(t)
		t.Setenv("GITEE_REVIEW_TIMEOUT_MIN", "0")
		if _, err := LoadConfig(""); err == nil || !strings.Contains(err.Error(), "GITEE_REVIEW_TIMEOUT_MIN") {
			t.Fatalf("want a timeout error, got: %v", err)
		}
	})
	t.Run("concurrency", func(t *testing.T) {
		baseEnv(t)
		t.Setenv("GITEE_CONCURRENCY", "-1")
		if _, err := LoadConfig(""); err == nil || !strings.Contains(err.Error(), "GITEE_CONCURRENCY") {
			t.Fatalf("want a concurrency error, got: %v", err)
		}
	})
}

// The whole point of the credential-injection design: `docker run -e` must
// override the .env baked into the image, not the other way round.
func TestLoadConfigEnvironmentWinsOverFile(t *testing.T) {
	baseEnv(t)
	path := writeEnvFile(t, "GITEE_TOKEN=from-file\nGITEE_ADDR=:9999\n")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GiteeToken != "token-value" {
		t.Errorf("GiteeToken = %q, want the environment value to win", cfg.GiteeToken)
	}
	// A key present only in the file is still honoured.
	if cfg.Addr != ":9999" {
		t.Errorf("Addr = %q, want the file value", cfg.Addr)
	}
}

// An explicitly empty environment variable shadows the file rather than
// falling back to it. For a credential that is the safer reading: `docker run
// -e GITEE_TOKEN=` is an operator saying "no token", and silently posting as
// whichever token the image happens to carry would be the wrong answer.
func TestLoadConfigEmptyEnvironmentShadowsFile(t *testing.T) {
	baseEnv(t)
	t.Setenv("GITEE_TOKEN", "")
	path := writeEnvFile(t, "GITEE_TOKEN=from-file\n")

	if _, err := LoadConfig(path); err == nil {
		t.Fatal("want an error: an explicitly empty token must not fall back to the file")
	}
}

func TestLoadConfigFileSuppliesEverything(t *testing.T) {
	for _, key := range []string{"GITEE_TOKEN", "GITEE_WEBHOOK_SECRET", "OCR_LLM_URL", "OCR_LLM_TOKEN", "OCR_LLM_MODEL"} {
		// Register the variable for restoration first, then remove it: the
		// loader only consults the file for variables that are absent, so
		// leaving them present-but-empty would shadow the file under test.
		t.Setenv(key, "sentinel")
		os.Unsetenv(key)
	}
	path := writeEnvFile(t, strings.Join([]string{
		"GITEE_TOKEN=file-token",
		"GITEE_WEBHOOK_SECRET=file-secret",
		"OCR_LLM_URL=https://file.example/v1",
		"OCR_LLM_TOKEN=file-llm",
		"OCR_LLM_MODEL=file-model",
		"OCR_USE_ANTHROPIC=true",
		"OCR_LLM_PROTOCOL=openai",
		"GITEE_DRY_RUN=false",
		"GITEE_VERIFY_DEPS=false",
		"GITEE_CONCURRENCY=3",
		"GITEE_REVIEW_TIMEOUT_MIN=42",
	}, "\n"))

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GiteeToken != "file-token" || cfg.LLMModel != "file-model" {
		t.Fatalf("file values not applied: %+v", cfg)
	}
	if cfg.DryRun || cfg.VerifyDeps {
		t.Error("explicit false values should be honoured, not replaced by defaults")
	}
	if cfg.Concurrency != 3 || cfg.ReviewTimeoutMin != 42 {
		t.Errorf("numeric overrides not applied: %+v", cfg)
	}
}

// A typo in a switch should not stop the service; a typo in a required
// credential should. These two policies are deliberately different.
func TestBoolOrFallsBackOnUnknownSpelling(t *testing.T) {
	for _, value := range []string{"1", "true", "TRUE", " yes ", "on"} {
		if !boolOr(value, false) {
			t.Errorf("boolOr(%q) should be true", value)
		}
	}
	for _, value := range []string{"0", "false", "NO", " off "} {
		if boolOr(value, true) {
			t.Errorf("boolOr(%q) should be false", value)
		}
	}
	if !boolOr("maybe", true) {
		t.Error("an unrecognised value should keep the fallback")
	}
	if boolOr("maybe", false) {
		t.Error("an unrecognised value should keep a false fallback")
	}
}

func TestIntOrFallsBackOnGarbage(t *testing.T) {
	if got := intOr("12", 5); got != 12 {
		t.Errorf("intOr(\"12\") = %d, want 12", got)
	}
	if got := intOr(" 7 ", 5); got != 7 {
		t.Errorf("intOr should tolerate surrounding spaces, got %d", got)
	}
	if got := intOr("abc", 5); got != 5 {
		t.Errorf("intOr(\"abc\") = %d, want the fallback 5", got)
	}
}

func TestValueOrTrimsAndFallsBack(t *testing.T) {
	if got := valueOr("  x  ", "d"); got != "x" {
		t.Errorf("valueOr = %q, want %q", got, "x")
	}
	if got := valueOr("   ", "d"); got != "d" {
		t.Errorf("blank should fall back, got %q", got)
	}
}

func TestMaskNeverRevealsTheSecret(t *testing.T) {
	if got := mask(""); got != "<unset>" {
		t.Errorf("mask(\"\") = %q", got)
	}
	// Not one byte of the secret, at any length. A suffix is the part of a
	// credential that confirms a guess, and the length alone already answers the
	// question the log line is asked -- whether the right value was loaded at
	// all, or a paste was truncated.
	secret := "sk-abcdefghijklmnop"
	for _, candidate := range []string{"abcd", secret, secret + secret} {
		want := fmt.Sprintf("<set, %d chars>", len(candidate))
		got := mask(candidate)
		if got != want {
			t.Errorf("mask(%q) = %q, want %q", candidate, got, want)
		}
		if strings.Contains(got, candidate) {
			t.Errorf("mask leaked the secret: %q", got)
		}
	}
}

func TestRedactedHidesEverySecret(t *testing.T) {
	baseEnv(t)
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rendered := cfg.Redacted()
	for name, secret := range map[string]string{
		"gitee token":    cfg.GiteeToken,
		"webhook secret": cfg.WebhookSecret,
		"llm token":      cfg.LLMToken,
	} {
		if strings.Contains(rendered, secret) {
			t.Errorf("Redacted leaked the %s: %q", name, rendered)
		}
	}
}

// The ocr subprocess needs the LLM credentials and nothing else. Handing it
// the parent environment would pass GITEE_TOKEN to a program with no use for
// it, widening where that credential can leak from.
func TestLLMEnvExcludesGiteeCredentials(t *testing.T) {
	baseEnv(t)
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	env := cfg.LLMEnv([]string{"PATH=/usr/bin", "GITEE_TOKEN=leaked", "GITEE_WEBHOOK_SECRET=leaked"})
	joined := strings.Join(env, "\n")
	for _, want := range []string{"OCR_LLM_URL=", "OCR_LLM_TOKEN=", "OCR_LLM_MODEL="} {
		if !strings.Contains(joined, want) {
			t.Errorf("LLMEnv should carry %s", want)
		}
	}
	// The credential that can write to every repository the operator can must
	// not be inherited by a process that spends minutes parsing a merge request
	// nobody has vouched for.
	if strings.Contains(joined, "GITEE_TOKEN") || strings.Contains(joined, "GITEE_WEBHOOK_SECRET") {
		t.Errorf("Gitee credentials reached the ocr subprocess:\n%s", joined)
	}
	// The unrelated parts of the environment stay, or ocr loses the PATH it
	// needs to run git.
	if !strings.Contains(joined, "PATH=/usr/bin") {
		t.Error("the rest of the environment should be passed through")
	}
	if strings.Count(joined, "OCR_LLM_TOKEN=") != 1 {
		t.Errorf("each LLM variable should be set exactly once:\n%s", joined)
	}
}

func TestLLMEnvOmitsEmptyOptionalVariables(t *testing.T) {
	baseEnv(t)
	for _, key := range []string{"OCR_LLM_PROTOCOL", "OCR_USE_ANTHROPIC"} {
		os.Unsetenv(key)
		t.Setenv(key, "")
	}
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	joined := strings.Join(cfg.LLMEnv(nil), "\n")
	if strings.Contains(joined, "OCR_LLM_PROTOCOL=") || strings.Contains(joined, "OCR_USE_ANTHROPIC=") {
		t.Errorf("empty optional variables should be omitted, got:\n%s", joined)
	}
}
