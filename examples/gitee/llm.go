// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// llmProtocol is the wire format the configured endpoint speaks.
type llmProtocol int

const (
	// protocolUnsupported covers the formats this service's judge does not
	// implement. It is not an error at startup: verification is skipped and
	// every finding is kept, which is the same thing that happens when the
	// endpoint is unreachable.
	protocolUnsupported llmProtocol = iota
	protocolAnthropic
	protocolOpenAIChat
)

// resolveJudgeProtocol mirrors ocr's own resolution, including its default.
//
// The default matters: with neither variable set ocr speaks the Anthropic
// protocol, so a judge that assumed OpenAI would send the wrong body to the
// same endpoint and fail on every candidate. Keeping the two in step is the
// whole reason this function exists rather than reading one variable.
func resolveJudgeProtocol(protocolEnv, useAnthropicEnv string) llmProtocol {
	if protocol := strings.ToLower(strings.TrimSpace(protocolEnv)); protocol != "" {
		switch protocol {
		case "anthropic":
			return protocolAnthropic
		case "openai":
			return protocolOpenAIChat
		default:
			// "openai-responses" and "anthropic-bedrock" are real protocols
			// that this judge does not implement.
			return protocolUnsupported
		}
	}
	useAnthropic := true
	if value := strings.TrimSpace(useAnthropicEnv); value != "" {
		lower := strings.ToLower(value)
		useAnthropic = lower == "true" || lower == "1" || lower == "yes"
	}
	if useAnthropic {
		return protocolAnthropic
	}
	return protocolOpenAIChat
}

// verdict is the judge's answer about one finding.
type verdict struct {
	// Contradicted is true only when the evidence shows the finding's claim is
	// false.
	Contradicted bool `json:"contradicted"`
	// Reason cites the evidence line that decides it. A verdict without a
	// reason is treated as no verdict at all: the point of this round is that a
	// finding is only dropped on the strength of a named line.
	Reason string `json:"reason"`
}

// judge decides whether a finding survives the evidence about a dependency.
type judge interface {
	Judge(ctx context.Context, request verifyRequest) (verdict, error)
}

// judgeClient is a minimal chat client for the two wire formats ocr itself
// speaks. Its request bodies are hand-written rather than taken from an SDK so
// the service keeps its no-dependency property; the fields used are the small
// intersection both APIs need.
type judgeClient struct {
	endpoint string
	protocol llmProtocol
	token    string
	model    string
	doer     httpDoer
	// maxTokens bounds the answer, which is one small JSON object.
	maxTokens int
	// logf carries the detail that must not reach the error. An error from this
	// client is quoted in the summary comment posted on the merge request, so
	// anything operator-facing -- the configured endpoint, an upstream body --
	// is logged instead of returned.
	logf func(format string, args ...any)
}

// newJudgeClient returns a judge for the configured endpoint, or nil when the
// endpoint speaks a protocol this service does not implement.
func newJudgeClient(cfg *Config, doer httpDoer, logf func(string, ...any)) judge {
	protocol := resolveJudgeProtocol(cfg.LLMProtocol, cfg.LLMUseAnthropic)
	if protocol == protocolUnsupported {
		// Named loudly because it turns the feature off: an operator who
		// configured openai-responses should not have to infer from silence
		// that verification never ran.
		logf("dependency verification is unavailable: this service implements the anthropic and openai chat protocols, "+
			"and %q is set to %q; findings will be kept unverified",
			"OCR_LLM_PROTOCOL", cfg.LLMProtocol)
		return nil
	}
	return &judgeClient{
		endpoint:  judgeEndpoint(protocol, cfg.LLMURL),
		protocol:  protocol,
		token:     cfg.LLMToken,
		model:     cfg.LLMModel,
		doer:      doer,
		maxTokens: 512,
		logf:      logf,
	}
}

// judgeEndpoint resolves the path the protocol's API lives at, the way ocr
// resolves it, so an OCR_LLM_URL meant for ocr is usable here unchanged.
//
// Mirroring ocr matters for the Anthropic case in particular: a gateway URL of
// "https://api.anthropic.com/v1" is one ocr itself accepts and turns into
// ".../v1/messages", while appending the full suffix blindly would produce
// ".../v1/v1/messages" and fail every call. See ensureMessagesSuffix in
// internal/llm/resolver.go.
func judgeEndpoint(protocol llmProtocol, rawURL string) string {
	base := strings.TrimRight(strings.TrimSpace(rawURL), "/")
	switch protocol {
	case protocolAnthropic:
		switch {
		case strings.HasSuffix(base, "/v1/messages"):
			return base
		case strings.HasSuffix(base, "/v1"):
			return base + "/messages"
		case strings.Contains(base, "/v1/"):
			// Already versioned, e.g. ".../v1/anthropic".
			return base
		default:
			return base + "/v1/messages"
		}
	default:
		// ocr appends this one unconditionally when it is missing; there is no
		// versioned form it has to recognise.
		if strings.HasSuffix(base, "/chat/completions") {
			return base
		}
		return base + "/chat/completions"
	}
}

// Judge asks the configured model whether the evidence contradicts a finding.
func (c *judgeClient) Judge(ctx context.Context, request verifyRequest) (verdict, error) {
	body, err := c.requestBody(request)
	if err != nil {
		return verdict{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return verdict{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	if c.protocol == protocolAnthropic {
		httpReq.Header.Set("anthropic-version", "2023-06-01")
	}

	resp, err := c.doer.Do(httpReq)
	if err != nil {
		return verdict{}, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return verdict{}, err
	}
	if resp.StatusCode != http.StatusOK {
		// The endpoint and the body are logged, not returned. This error reaches
		// uncheckedNote, and that note is posted in the summary comment on the
		// merge request: an operator's gateway URL -- which may be an internal
		// host -- and an arbitrary upstream body do not belong on a public page.
		c.logf("judge endpoint %s answered %d: %s",
			c.endpoint, resp.StatusCode, clip(strings.TrimSpace(string(payload)), 300))
		return verdict{}, fmt.Errorf("the model endpoint answered %d", resp.StatusCode)
	}

	text, err := c.replyText(payload)
	if err != nil {
		return verdict{}, err
	}
	return parseVerdict(text)
}

func (c *judgeClient) requestBody(request verifyRequest) ([]byte, error) {
	system, user := judgePrompt(request)
	if c.protocol == protocolAnthropic {
		return json.Marshal(map[string]any{
			"model":      c.model,
			"max_tokens": c.maxTokens,
			"system":     system,
			"messages": []map[string]string{
				{"role": "user", "content": user},
			},
		})
	}
	return json.Marshal(map[string]any{
		"model":       c.model,
		"temperature": 0,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
}

// replyText pulls the assistant's message out of either response shape.
func (c *judgeClient) replyText(payload []byte) (string, error) {
	if c.protocol == protocolAnthropic {
		var response struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(payload, &response); err != nil {
			return "", fmt.Errorf("decode anthropic response: %w", err)
		}
		if response.Error != nil {
			return "", fmt.Errorf("anthropic error: %s", response.Error.Message)
		}
		for _, block := range response.Content {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				return block.Text, nil
			}
		}
		return "", fmt.Errorf("anthropic response carried no text block")
	}

	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		return "", fmt.Errorf("decode openai response: %w", err)
	}
	if response.Error != nil {
		return "", fmt.Errorf("openai error: %s", response.Error.Message)
	}
	if len(response.Choices) == 0 {
		return "", fmt.Errorf("openai response carried no choices")
	}
	return response.Choices[0].Message.Content, nil
}

// parseVerdict reads the JSON object out of the model's reply.
//
// The first '{' is the start rather than a strict decode, because a model that
// prefixes its answer with a sentence -- or wraps it in a fenced block -- is
// answering the question, and discarding that answer over formatting would send
// the finding back to the reader unexamined for no reason.
func parseVerdict(text string) (verdict, error) {
	trimmed := strings.TrimSpace(text)
	start := strings.IndexByte(trimmed, '{')
	end := strings.LastIndexByte(trimmed, '}')
	if start < 0 || end <= start {
		return verdict{}, fmt.Errorf("the judge's reply was not a JSON object: %s", clip(trimmed, 200))
	}
	var parsed verdict
	if err := json.Unmarshal([]byte(trimmed[start:end+1]), &parsed); err != nil {
		return verdict{}, fmt.Errorf("decode the judge's verdict: %w", err)
	}
	return parsed, nil
}

// judgePrompt builds the question. It is written to make a drop hard and a keep
// easy, because the two mistakes are not equally costly: keeping a false
// finding costs a reader a minute, and dropping a true one hides a defect.
func judgePrompt(request verifyRequest) (system, user string) {
	system = "You check one code review finding against evidence about a third-party package. " +
		"Answer with a single JSON object and nothing else.\n\n" +
		"The finding was written by a reviewer who could see only the diff, which does not contain " +
		"the package's source, so a claim about the package's API may be wrong.\n\n" +
		"Reply {\"contradicted\": true, \"reason\": \"...\"} only when a line quoted below directly shows " +
		"the finding's claim to be false. Cite that line in the reason.\n\n" +
		"Reply {\"contradicted\": false, \"reason\": \"...\"} in every other case: when the lines are about " +
		"a different matter, when the finding is not making a factual claim about the package's API, " +
		"when the evidence is incomplete, and when you are unsure. A finding you cannot settle is kept."

	var b strings.Builder
	b.WriteString("Finding:\n")
	b.WriteString(strings.TrimSpace(request.Finding))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "It was made against a revision that pins %s@%s.\n", request.DependencyName, request.DependencyVersion)
	fmt.Fprintf(&b, "Examined: %s (complete: %t).\n", request.Surface, request.SurfaceComplete)
	b.WriteString("\nLines in that surface mentioning the identifiers the finding names:\n")
	for _, member := range request.Members {
		fmt.Fprintf(&b, "\n### `%s`\n", member.Name)
		if len(member.Lines) == 0 {
			b.WriteString("- no line mentions it\n")
			continue
		}
		for _, line := range member.Lines {
			fmt.Fprintf(&b, "- %s\n", line)
		}
	}
	return system, b.String()
}
