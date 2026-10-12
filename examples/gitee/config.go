// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the service's entire configuration surface. Everything comes from
// the environment so the container stays stateless: the same image runs
// anywhere and only the injected .env differs.
type Config struct {
	// Gitee connection.
	GiteeAPIBase  string
	GiteeToken    string
	WebhookSecret string
	// GitUsername is the login git authenticates with over HTTPS. Gitee pairs
	// the account name with a personal access token as the password, so the
	// default placeholder is only right when the server accepts it.
	GitUsername string

	// LLM credentials, forwarded verbatim to the ocr subprocess. They live here
	// rather than in ocr's own config file so a container restart cannot lose
	// them and the image never bakes a credential in.
	LLMURL          string
	LLMToken        string
	LLMModel        string
	LLMUseAnthropic string
	LLMProtocol     string

	// Service behaviour.
	Addr             string
	ReviewTimeoutMin int
	Concurrency      int
	VerifyDeps       bool
	DryRun           bool

	// Runtime locations.
	OcrBin   string
	WorkRoot string
}

// Defaults chosen so a bare `docker run` with only the required variables set
// behaves safely: nothing is posted until DryRun is explicitly turned off.
const (
	defaultGiteeAPIBase  = "https://gitee.com/api/v5"
	defaultAddr          = ":8080"
	defaultReviewTimeout = 15
	defaultConcurrency   = 8
	defaultOcrBin        = "ocr"
)

// LoadConfig reads configuration from the environment, after layering in a
// .env file when one is present.
//
// Precedence is real environment first, .env second: an operator who exports a
// variable or passes `docker run -e` expects that to win over a file baked into
// the image, which is the opposite of what most dotenv loaders do.
func LoadConfig(envFile string) (*Config, error) {
	fileVars, err := readEnvFile(envFile)
	if err != nil {
		return nil, err
	}
	lookup := func(key string) string {
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		return fileVars[key]
	}

	cfg := &Config{
		GiteeAPIBase:     valueOr(lookup("GITEE_API_BASE"), defaultGiteeAPIBase),
		GiteeToken:       strings.TrimSpace(lookup("GITEE_TOKEN")),
		WebhookSecret:    lookup("GITEE_WEBHOOK_SECRET"),
		GitUsername:      strings.TrimSpace(lookup("GITEE_GIT_USERNAME")),
		LLMURL:           strings.TrimSpace(lookup("OCR_LLM_URL")),
		LLMToken:         lookup("OCR_LLM_TOKEN"),
		LLMModel:         strings.TrimSpace(lookup("OCR_LLM_MODEL")),
		LLMUseAnthropic:  strings.TrimSpace(lookup("OCR_USE_ANTHROPIC")),
		LLMProtocol:      strings.TrimSpace(lookup("OCR_LLM_PROTOCOL")),
		Addr:             valueOr(lookup("GITEE_ADDR"), defaultAddr),
		OcrBin:           valueOr(lookup("OCR_BIN"), defaultOcrBin),
		WorkRoot:         valueOr(lookup("GITEE_WORK_ROOT"), ""),
		VerifyDeps:       boolOr(lookup("GITEE_VERIFY_DEPS"), true),
		DryRun:           boolOr(lookup("GITEE_DRY_RUN"), true),
		ReviewTimeoutMin: intOr(lookup("GITEE_REVIEW_TIMEOUT_MIN"), defaultReviewTimeout),
		Concurrency:      intOr(lookup("GITEE_CONCURRENCY"), defaultConcurrency),
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validate reports every missing or nonsensical setting at once. Failing at
// startup with the full list beats discovering each one on the first webhook,
// when the service is already live and silently dropping work.
func (c *Config) validate() error {
	var missing []string
	require := func(name, value string) {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	require("GITEE_TOKEN", c.GiteeToken)
	require("GITEE_WEBHOOK_SECRET", c.WebhookSecret)
	require("OCR_LLM_URL", c.LLMURL)
	require("OCR_LLM_TOKEN", c.LLMToken)
	require("OCR_LLM_MODEL", c.LLMModel)
	if len(missing) > 0 {
		return fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	if c.ReviewTimeoutMin <= 0 {
		return fmt.Errorf("GITEE_REVIEW_TIMEOUT_MIN must be positive, got %d", c.ReviewTimeoutMin)
	}
	if c.Concurrency <= 0 {
		return fmt.Errorf("GITEE_CONCURRENCY must be positive, got %d", c.Concurrency)
	}
	return nil
}

// LLMEnv is the environment handed to the ocr subprocess. The service's own
// credentials are removed from it: ocr runs for minutes over a merge request it
// does not trust, and a token that can write to every repository the operator
// can is not something it has any use for. Everything else is passed through,
// because ocr shells out to git and needs the environment that makes it work.
func (c *Config) LLMEnv(base []string) []string {
	env := make([]string, 0, len(base)+5)
	for _, entry := range base {
		if strings.HasPrefix(entry, "GITEE_") {
			continue
		}
		env = append(env, entry)
	}
	set := func(key, value string) {
		if value != "" {
			env = append(env, key+"="+value)
		}
	}
	set("OCR_LLM_URL", c.LLMURL)
	set("OCR_LLM_TOKEN", c.LLMToken)
	set("OCR_LLM_MODEL", c.LLMModel)
	set("OCR_USE_ANTHROPIC", c.LLMUseAnthropic)
	set("OCR_LLM_PROTOCOL", c.LLMProtocol)
	return env
}

// Redacted renders the configuration for startup logging. Tokens are reduced to
// a length and a suffix so an operator can confirm the right secret is loaded
// without the value landing in a log aggregator.
func (c *Config) Redacted() string {
	return fmt.Sprintf(
		"gitee_api=%s gitee_token=%s webhook_secret=%s llm_url=%s llm_model=%s "+
			"addr=%s timeout_min=%d concurrency=%d verify_deps=%t dry_run=%t",
		c.GiteeAPIBase, mask(c.GiteeToken), mask(c.WebhookSecret), c.LLMURL, c.LLMModel,
		c.Addr, c.ReviewTimeoutMin, c.Concurrency, c.VerifyDeps, c.DryRun)
}

// mask reports that a secret is set and how long it is without disclosing any
// of it. The length is what diagnoses the two failures that actually happen --
// a truncated paste and an empty one -- and a suffix would hand the log reader
// the part of the secret that confirms a guess.
func mask(secret string) string {
	if secret == "" {
		return "<unset>"
	}
	return fmt.Sprintf("<set, %d chars>", len(secret))
}

// readEnvFile parses a dotenv file. A missing file is not an error: inside a
// container the variables usually arrive as real environment, and the loader
// should not insist on a file that is meant to be optional.
func readEnvFile(path string) (map[string]string, error) {
	vars := map[string]string{}
	if strings.TrimSpace(path) == "" {
		return vars, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return vars, nil
		}
		return nil, fmt.Errorf("open env file %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		key, value, ok := parseEnvLine(scanner.Text())
		if !ok {
			continue
		}
		if _, exists := vars[key]; exists {
			return nil, fmt.Errorf("%s:%d: duplicate key %s", path, lineNo, key)
		}
		vars[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read env file %s: %w", path, err)
	}
	return vars, nil
}

// parseEnvLine handles one dotenv line: `KEY=VALUE`, with optional `export`,
// blank lines and `#` comments ignored.
func parseEnvLine(raw string) (key, value string, ok bool) {
	line := strings.TrimSpace(raw)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimPrefix(line, "export ")
	eq := strings.IndexByte(line, '=')
	if eq <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(line[:eq])
	value = strings.TrimSpace(line[eq+1:])

	// A quoted value is taken literally, so a secret containing `#` or
	// surrounding spaces survives intact. Anything after the closing quote has
	// to be a comment or nothing: `KEY="abc" # prod` is the secret abc, and
	// keeping the quotes would send a credential with quote characters in it and
	// fail every request with a 401.
	if len(value) >= 1 && (value[0] == '"' || value[0] == '\'') {
		quote := value[0]
		end := strings.IndexByte(value[1:], quote)
		if end < 0 {
			// An unterminated quote is a typo, and the value it would otherwise
			// produce carries the opening quote into the credential: every
			// request then fails with a 401 that says nothing about the cause.
			// Dropping the line instead reports the variable as unset, which
			// names it.
			return "", "", false
		}
		rest := strings.TrimSpace(value[end+2:])
		if rest == "" || strings.HasPrefix(rest, "#") {
			return key, value[1 : end+1], true
		}
	}
	// For anything else a whitespace-preceded `#` starts a trailing comment,
	// which is how the shipped .env.example documents each key.
	if i := strings.Index(value, " #"); i >= 0 {
		value = strings.TrimSpace(value[:i])
	}
	return key, value, true
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

// boolOr accepts the spellings an operator is likely to reach for. Anything
// unrecognised falls back rather than erroring, because a typo in a service
// switch should not stop the service from starting.
func boolOr(value string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func intOr(value string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	return n
}
