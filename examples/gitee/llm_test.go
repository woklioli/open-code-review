// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The default matters more than the explicit settings: ocr speaks Anthropic
// when neither variable is set, so a judge that assumed OpenAI would send the
// wrong request body to the same endpoint and fail on every candidate.
func TestResolveJudgeProtocol(t *testing.T) {
	cases := map[string]struct {
		protocolEnv, useAnthropicEnv string
		want                         llmProtocol
	}{
		"nothing set defaults to anthropic":  {"", "", protocolAnthropic},
		"explicit anthropic":                 {"anthropic", "", protocolAnthropic},
		"explicit openai":                    {"openai", "", protocolOpenAIChat},
		"protocol beats the legacy flag":     {"openai", "true", protocolOpenAIChat},
		"padded and cased":                   {" OpenAI ", "", protocolOpenAIChat},
		"responses is not implemented":       {"openai-responses", "", protocolUnsupported},
		"bedrock is not implemented":         {"anthropic-bedrock", "", protocolUnsupported},
		"a typo is not a guess":              {"opnai", "", protocolUnsupported},
		"legacy flag false":                  {"", "false", protocolOpenAIChat},
		"legacy flag true":                   {"", "true", protocolAnthropic},
		"legacy flag one":                    {"", "1", protocolAnthropic},
		"legacy flag off":                    {"", "0", protocolOpenAIChat},
		"legacy flag yes":                    {"", "yes", protocolAnthropic},
		"legacy flag nonsense keeps default": {"", "maybe", protocolOpenAIChat},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := resolveJudgeProtocol(tc.protocolEnv, tc.useAnthropicEnv); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestJudgeEndpoint(t *testing.T) {
	cases := map[string]struct {
		protocol llmProtocol
		url      string
		want     string
	}{
		"anthropic bare host":    {protocolAnthropic, "https://gateway.example", "https://gateway.example/v1/messages"},
		"anthropic trailing":     {protocolAnthropic, "https://gateway.example/", "https://gateway.example/v1/messages"},
		"anthropic already full": {protocolAnthropic, "https://gateway.example/v1/messages", "https://gateway.example/v1/messages"},
		// This is the form ocr itself accepts, and appending the whole suffix
		// would produce ".../v1/v1/messages" and fail every call. ocr's own
		// ensureMessagesSuffix appends only "/messages" here.
		"anthropic versioned base": {protocolAnthropic, "https://api.anthropic.com/v1", "https://api.anthropic.com/v1/messages"},
		"anthropic versioned path": {protocolAnthropic, "https://gateway.example/v1/anthropic", "https://gateway.example/v1/anthropic"},
		"openai bare":              {protocolOpenAIChat, "https://gateway.example/v1", "https://gateway.example/v1/chat/completions"},
		"openai already full":      {protocolOpenAIChat, "https://gateway.example/v1/chat/completions", "https://gateway.example/v1/chat/completions"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := judgeEndpoint(tc.protocol, tc.url); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewJudgeClientSkipsUnsupportedProtocols(t *testing.T) {
	cfg := &Config{LLMURL: "https://gateway.example", LLMToken: "t", LLMModel: "m", LLMProtocol: "openai-responses"}
	var logs []string
	if got := newJudgeClient(cfg, &stubDoer{}, func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}); got != nil {
		t.Error("an unimplemented protocol should leave the judge unavailable")
	}
	// Silence here would let an operator believe verification ran.
	if !strings.Contains(strings.Join(logs, "\n"), "openai-responses") {
		t.Errorf("the reason should be logged, got %v", logs)
	}
}

func TestJudgeClientCallsAnthropic(t *testing.T) {
	var got struct {
		path        string
		auth        string
		version     string
		contentType string
		body        map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.auth = r.Header.Get("Authorization")
		got.version = r.Header.Get("anthropic-version")
		got.contentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"{\"contradicted\": true, \"reason\": \"` +
			"`retry` is declared in index.d.ts" + `\"}"}]}`))
	}))
	defer server.Close()

	client := &judgeClient{
		endpoint:  judgeEndpoint(protocolAnthropic, server.URL),
		protocol:  protocolAnthropic,
		token:     "secret",
		model:     "a-model",
		doer:      server.Client(),
		maxTokens: 512,
	}
	result, err := client.Judge(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Contradicted || !strings.Contains(result.Reason, "retry") {
		t.Errorf("verdict = %+v", result)
	}
	if got.path != "/v1/messages" {
		t.Errorf("path = %q", got.path)
	}
	if got.auth != "Bearer secret" {
		t.Errorf("Authorization = %q", got.auth)
	}
	if got.version != "2023-06-01" {
		t.Errorf("anthropic-version = %q", got.version)
	}
	if got.body["system"] == nil || got.body["max_tokens"] == nil {
		t.Errorf("body = %v", got.body)
	}
	// The evidence has to reach the model, or it is answering from memory.
	messages, _ := json.Marshal(got.body["messages"])
	if !strings.Contains(string(messages), "index.d.ts") {
		t.Errorf("the evidence was not sent: %s", messages)
	}
}

func TestJudgeClientCallsOpenAI(t *testing.T) {
	var path, auth string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"contradicted\": false, \"reason\": \"unclear\"}"}}]}`))
	}))
	defer server.Close()

	client := &judgeClient{
		endpoint: judgeEndpoint(protocolOpenAIChat, server.URL+"/v1"),
		protocol: protocolOpenAIChat,
		token:    "secret",
		model:    "a-model",
		doer:     server.Client(),
	}
	result, err := client.Judge(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Contradicted {
		t.Errorf("verdict = %+v", result)
	}
	if path != "/v1/chat/completions" {
		t.Errorf("path = %q", path)
	}
	if auth != "Bearer secret" {
		t.Errorf("Authorization = %q", auth)
	}
	// The system prompt has to travel as a message, since this protocol has no
	// separate system field.
	messages, _ := json.Marshal(body["messages"])
	if !strings.Contains(string(messages), "contradicted") {
		t.Errorf("the instructions were not sent: %s", messages)
	}
}

func TestJudgeClientReportsFailures(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		wantIn string
	}{
		"http error":        {http.StatusUnauthorized, `{"message":"bad key"}`, "401"},
		"anthropic error":   {http.StatusOK, `{"error":{"message":"overloaded"}}`, "overloaded"},
		"no content blocks": {http.StatusOK, `{"content":[]}`, "no text block"},
		"not json":          {http.StatusOK, `plainly not json`, "decode anthropic response"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := &judgeClient{
				endpoint: server.URL, protocol: protocolAnthropic, token: "t", model: "m",
				doer: server.Client(), logf: func(string, ...any) {},
			}
			if _, err := client.Judge(context.Background(), sampleRequest()); err == nil {
				t.Fatal("want an error")
			} else if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantIn)
			}
		})
	}
}

// The error this client returns is quoted in the summary comment posted on the
// merge request. The configured endpoint -- which on a self-hosted deployment is
// an internal host -- and the upstream body belong in the log, not on that page.
func TestJudgeFailureKeepsOperatorDetailOutOfTheError(t *testing.T) {
	const body = `{"message":"bad key for the internal gateway"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	var logged []string
	client := &judgeClient{
		endpoint: server.URL, protocol: protocolAnthropic, token: "t", model: "m",
		doer: server.Client(),
		logf: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
	}

	_, err := client.Judge(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), server.URL) {
		t.Errorf("the error names the configured endpoint: %v", err)
	}
	if strings.Contains(err.Error(), "bad key") {
		t.Errorf("the error quotes the upstream body: %v", err)
	}
	detail := strings.Join(logged, "\n")
	if !strings.Contains(detail, server.URL) || !strings.Contains(detail, "bad key") {
		t.Errorf("the log should carry the endpoint and the body, got: %s", detail)
	}
}

func TestJudgeClientReportsOpenAIFailures(t *testing.T) {
	cases := map[string]struct {
		body   string
		wantIn string
	}{
		"error object": {`{"error":{"message":"rate limited"}}`, "rate limited"},
		"no choices":   {`{"choices":[]}`, "no choices"},
		"not json":     {`nonsense`, "decode openai response"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client := &judgeClient{
				endpoint: server.URL, protocol: protocolOpenAIChat, token: "t", model: "m",
				doer: server.Client(), logf: func(string, ...any) {},
			}
			if _, err := client.Judge(context.Background(), sampleRequest()); err == nil {
				t.Fatal("want an error")
			} else if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantIn)
			}
		})
	}
}

func TestJudgeClientReportsATransportFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := server.URL
	server.Close()
	client := &judgeClient{endpoint: endpoint, protocol: protocolAnthropic, doer: http.DefaultClient}
	if _, err := client.Judge(context.Background(), sampleRequest()); err == nil {
		t.Fatal("want a transport error")
	}
}

// A model that wraps its answer in prose or a fenced block is still answering
// the question, and discarding that would send the finding back to the reader
// unexamined for no reason.
func TestParseVerdict(t *testing.T) {
	cases := map[string]struct {
		text string
		want verdict
	}{
		"plain":               {`{"contradicted":true,"reason":"retry is declared"}`, verdict{true, "retry is declared"}},
		"fenced":              {"```json\n{\"contradicted\": false, \"reason\": \"unclear\"}\n```", verdict{false, "unclear"}},
		"prefixed with prose": {"Looking at this, {\"contradicted\": true, \"reason\": \"retry exists\"} — so it is wrong.", verdict{true, "retry exists"}},
		"extra fields":        {`{"contradicted":true,"reason":"x","confidence":0.9}`, verdict{true, "x"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parseVerdict(tc.text)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseVerdictRejectsWhatIsNotAVerdict(t *testing.T) {
	for name, text := range map[string]string{
		"empty":         "",
		"prose only":    "I cannot answer that.",
		"unterminated":  `{"contradicted": true`,
		"not an object": "[]",
		"bad json":      `{"contradicted": tru}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseVerdict(text); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestJudgePromptCarriesTheEvidence(t *testing.T) {
	request := sampleRequest()
	system, user := judgePrompt(request)
	for _, want := range []string{"contradicted", "kept", "JSON"} {
		if !strings.Contains(system, want) {
			t.Errorf("the instructions should mention %q:\n%s", want, system)
		}
	}
	for _, want := range []string{"next@16.3.0", "index.d.ts:2", "no line mentions it", "complete: true"} {
		if !strings.Contains(user, want) {
			t.Errorf("the question should carry %q:\n%s", want, user)
		}
	}
}

func sampleRequest() verifyRequest {
	return verifyRequest{
		Finding:           retryPropFinding,
		DependencyName:    "next",
		DependencyVersion: "16.3.0",
		Surface:           "type declarations published by next@16.3.0",
		SurfaceComplete:   true,
		Members: []memberEvidence{
			{Name: "error", Lines: []string{"/dist/error.d.ts:1: declare function useError(): Error"}},
			{Name: "retry", Lines: []string{"/dist/index.d.ts:2: retry?: () => void"}},
			{Name: "FrontendErrorFallback"},
		},
	}
}
