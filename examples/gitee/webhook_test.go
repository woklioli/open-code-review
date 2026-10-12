// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestVerifyRequestAcceptsTokenHeader(t *testing.T) {
	header := http.Header{}
	header.Set("X-Gitee-Token", "s3cret")
	if err := VerifyRequest("s3cret", header, []byte("body")); err != nil {
		t.Fatalf("token header should verify: %v", err)
	}
}

func TestVerifyRequestAcceptsSignatureHeaders(t *testing.T) {
	body := []byte(`{"action":"open"}`)
	digest := signBody("s3cret", body)

	for _, tc := range []struct {
		name   string
		header string
		value  string
	}{
		{"bare digest", "X-Gitee-Signature", digest},
		{"prefixed digest", "X-Hub-Signature-256", "sha256=" + digest},
		{"uppercase digest", "X-Hub-Signature", strings.ToUpper(digest)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := http.Header{}
			header.Set(tc.header, tc.value)
			if err := VerifyRequest("s3cret", header, body); err != nil {
				t.Fatalf("%s should verify: %v", tc.header, err)
			}
		})
	}
}

// Every rejection reads the same to the caller. Distinguishing "wrong secret"
// from "no secret at all" tells an attacker which half of the guess was right.
func TestVerifyRequestRejectionsAreIndistinguishable(t *testing.T) {
	body := []byte(`{"action":"open"}`)
	withWrongToken := http.Header{}
	withWrongToken.Set("X-Gitee-Token", "nope")
	withWrongSig := http.Header{}
	withWrongSig.Set("X-Gitee-Signature", signBody("other", body))

	var messages []string
	for _, header := range []http.Header{withWrongToken, withWrongSig} {
		err := VerifyRequest("s3cret", header, body)
		if err == nil {
			t.Fatal("a wrong secret must not verify")
		}
		messages = append(messages, err.Error())
	}
	if messages[0] != messages[1] {
		t.Errorf("rejections differ:\n  %s\n  %s", messages[0], messages[1])
	}
}

func TestVerifyRequestRejects(t *testing.T) {
	body := []byte(`{"action":"open"}`)

	t.Run("no header at all", func(t *testing.T) {
		err := VerifyRequest("s3cret", http.Header{}, body)
		if err == nil {
			t.Fatal("want a rejection")
		}
		// The message names the headers it looked for: an operator whose hook
		// uses a spelling this service does not know needs to see them.
		for _, name := range []string{"X-Gitee-Token", "X-Gitee-Signature"} {
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error should name %s, got: %v", name, err)
			}
		}
	})

	t.Run("unconfigured secret", func(t *testing.T) {
		header := http.Header{}
		header.Set("X-Gitee-Token", "anything")
		if err := VerifyRequest("   ", header, body); err == nil {
			t.Fatal("an unset secret must fail closed rather than accept")
		}
	})

	// A signature that is valid for a different body must not authorise this
	// one; without this the body binding is decorative.
	t.Run("signature does not bind to the body", func(t *testing.T) {
		header := http.Header{}
		header.Set("X-Gitee-Signature", signBody("s3cret", []byte("different")))
		if err := VerifyRequest("s3cret", header, body); err == nil {
			t.Fatal("a signature for another body must not verify")
		}
	})

	t.Run("token header wins over signature", func(t *testing.T) {
		header := http.Header{}
		header.Set("X-Gitee-Token", "wrong")
		header.Set("X-Gitee-Signature", signBody("s3cret", body))
		if err := VerifyRequest("s3cret", header, body); err == nil {
			t.Fatal("a present-but-wrong token must not fall through to the signature")
		}
	})
}

// payload builds a merge request delivery. Keeping this in one place means a
// change to the shape under test is a one-line edit rather than a hunt.
func payload(overrides map[string]any) []byte {
	event := map[string]any{
		"action": "open",
		"number": 42,
		"pull_request": map[string]any{
			"number": 42,
			"title":  "Add the thing",
			"state":  "open",
			"head":   map[string]any{"ref": "feature", "sha": "aaaa1111"},
			"base":   map[string]any{"ref": "main", "sha": "bbbb2222"},
		},
		"repository": map[string]any{"full_name": "acme/widget"},
	}
	for k, v := range overrides {
		if v == nil {
			delete(event, k)
			continue
		}
		event[k] = v
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		panic(err)
	}
	return encoded
}

func TestParseEventHappyPath(t *testing.T) {
	job, err := ParseEvent(payload(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if job == nil {
		t.Fatal("want a job")
	}
	want := reviewJob{
		Owner: "acme", Repo: "widget", Number: 42,
		HeadSha: "aaaa1111", BaseSha: "bbbb2222",
		Title: "Add the thing", EventKind: "open",
	}
	if *job != want {
		t.Fatalf("got %+v, want %+v", *job, want)
	}
	if got := job.String(); got != "acme/widget#42" {
		t.Errorf("String() = %q", got)
	}
}

// The event vocabulary differs between Gitee's own pages, so a push to an open
// request must be recognised however it is spelled.
func TestParseEventAcceptsBothActionVocabularies(t *testing.T) {
	for _, action := range []string{"open", "opened", "update", "synchronize", "reopen", "reopened", "OPEN", " Update "} {
		t.Run(action, func(t *testing.T) {
			job, err := ParseEvent(payload(map[string]any{"action": action}))
			if err != nil || job == nil {
				t.Fatalf("action %q should start a review, got job=%v err=%v", action, job, err)
			}
		})
	}
}

func TestParseEventSkipsClosedAndMerged(t *testing.T) {
	for _, action := range []string{"close", "closed", "merge", "merged", "approve"} {
		t.Run(action, func(t *testing.T) {
			job, err := ParseEvent(payload(map[string]any{"action": action}))
			if err != nil {
				t.Fatalf("action %q should be a silent no-op, got error: %v", action, err)
			}
			if job != nil {
				t.Fatalf("action %q should not queue work, got %+v", action, job)
			}
		})
	}
}

// An action nobody has seen before is surfaced, not swallowed: a new Gitee
// action that silently does nothing would look like a healthy idle service.
func TestParseEventRejectsUnknownAction(t *testing.T) {
	_, err := ParseEvent(payload(map[string]any{"action": "teleported"}))
	if err == nil {
		t.Fatal("want an error for an unknown action")
	}
	if !strings.Contains(err.Error(), "teleported") {
		t.Errorf("error should quote the action, got: %v", err)
	}
}

func TestParseEventSkipsDrafts(t *testing.T) {
	cases := map[string]map[string]any{
		"draft flag":    {"pull_request": map[string]any{"number": 1, "draft": true, "head": map[string]any{"sha": "a"}, "base": map[string]any{"sha": "b"}}},
		"is_draft flag": {"pull_request": map[string]any{"number": 1, "is_draft": true, "head": map[string]any{"sha": "a"}, "base": map[string]any{"sha": "b"}}},
		"WIP title":     {"pull_request": map[string]any{"number": 1, "title": "WIP: still going", "head": map[string]any{"sha": "a"}, "base": map[string]any{"sha": "b"}}},
		"bracketed WIP": {"pull_request": map[string]any{"number": 1, "title": "[wip] scratch", "head": map[string]any{"sha": "a"}, "base": map[string]any{"sha": "b"}}},
	}
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			job, err := ParseEvent(payload(override))
			if err != nil {
				t.Fatalf("a draft should be a silent no-op, got error: %v", err)
			}
			if job != nil {
				t.Fatalf("a draft should not queue work, got %+v", job)
			}
		})
	}
}

// Gitee does not publish the payload schema, so the first real delivery is
// where an operator finds out what it looks like. The errors have to carry the
// keys that actually arrived.
func TestParseEventErrorsNameTheKeysThatArrived(t *testing.T) {
	t.Run("missing head sha", func(t *testing.T) {
		raw := payload(map[string]any{
			"pull_request": map[string]any{"number": 1, "base": map[string]any{"sha": "b"}},
		})
		_, err := ParseEvent(raw)
		if err == nil || !strings.Contains(err.Error(), "head.sha") {
			t.Fatalf("want a head.sha error, got: %v", err)
		}
		if !strings.Contains(err.Error(), "number") {
			t.Errorf("error should list the pull_request keys that did arrive, got: %v", err)
		}
	})

	t.Run("missing base sha", func(t *testing.T) {
		raw := payload(map[string]any{
			"pull_request": map[string]any{"number": 1, "head": map[string]any{"sha": "a"}},
		})
		_, err := ParseEvent(raw)
		if err == nil || !strings.Contains(err.Error(), "base.sha") {
			t.Fatalf("want a base.sha error, got: %v", err)
		}
	})

	t.Run("missing action lists top-level keys", func(t *testing.T) {
		_, err := ParseEvent(payload(map[string]any{"action": nil}))
		if err == nil || !strings.Contains(err.Error(), "action") {
			t.Fatalf("want an action error, got: %v", err)
		}
		if !strings.Contains(err.Error(), "repository") {
			t.Errorf("error should list the top-level keys, got: %v", err)
		}
	})

	t.Run("missing pull request object", func(t *testing.T) {
		_, err := ParseEvent(payload(map[string]any{"pull_request": nil}))
		if err == nil || !strings.Contains(err.Error(), "pull_request") {
			t.Fatalf("want a pull_request error, got: %v", err)
		}
	})

	t.Run("missing repository object", func(t *testing.T) {
		_, err := ParseEvent(payload(map[string]any{"repository": nil}))
		if err == nil || !strings.Contains(err.Error(), "repository") {
			t.Fatalf("want a repository error, got: %v", err)
		}
	})
}

func TestParseEventRejectsMalformedJSON(t *testing.T) {
	if _, err := ParseEvent([]byte("{not json")); err == nil {
		t.Fatal("want a decode error")
	} else if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error should say it was a decode failure, got: %v", err)
	}
}

// The event is named merge_requests_events, so a payload carrying the object
// under merge_request is accepted too rather than failing on a naming choice.
func TestParseEventAcceptsMergeRequestKey(t *testing.T) {
	raw := []byte(`{
		"action": "open",
		"merge_request": {"iid": 7, "head": {"sha": "a1"}, "base": {"sha": "b1"}},
		"repository": {"full_name": "acme/widget"}
	}`)
	job, err := ParseEvent(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if job.Number != 7 || job.HeadSha != "a1" {
		t.Fatalf("got %+v", job)
	}
}

func TestParseEventNumberFallbacks(t *testing.T) {
	t.Run("iid", func(t *testing.T) {
		raw := []byte(`{"action":"open","pull_request":{"iid":9,"head":{"sha":"a"},"base":{"sha":"b"}},"repository":{"full_name":"o/r"}}`)
		job, err := ParseEvent(raw)
		if err != nil || job.Number != 9 {
			t.Fatalf("got %+v err=%v", job, err)
		}
	})
	t.Run("top level number", func(t *testing.T) {
		raw := []byte(`{"action":"open","number":11,"pull_request":{"head":{"sha":"a"},"base":{"sha":"b"}},"repository":{"full_name":"o/r"}}`)
		job, err := ParseEvent(raw)
		if err != nil || job.Number != 11 {
			t.Fatalf("got %+v err=%v", job, err)
		}
	})
	t.Run("none present", func(t *testing.T) {
		raw := []byte(`{"action":"open","pull_request":{"head":{"sha":"a"},"base":{"sha":"b"}},"repository":{"full_name":"o/r"}}`)
		if _, err := ParseEvent(raw); err == nil || !strings.Contains(err.Error(), "number") {
			t.Fatalf("want a number error, got: %v", err)
		}
	})
}

func TestParseEventRepositoryFallbacks(t *testing.T) {
	t.Run("namespace as object with path", func(t *testing.T) {
		raw := []byte(`{"action":"open","pull_request":{"number":1,"head":{"sha":"a"},"base":{"sha":"b"}},
			"repository":{"namespace":{"path":"acme"},"path":"widget"}}`)
		job, err := ParseEvent(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if job.Owner != "acme" || job.Repo != "widget" {
			t.Fatalf("got %+v", job)
		}
	})

	t.Run("namespace as plain string", func(t *testing.T) {
		raw := []byte(`{"action":"open","pull_request":{"number":1,"head":{"sha":"a"},"base":{"sha":"b"}},
			"repository":{"namespace":"acme","name":"widget"}}`)
		job, err := ParseEvent(raw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if job.Owner != "acme" || job.Repo != "widget" {
			t.Fatalf("got %+v", job)
		}
	})

	t.Run("full_name without a slash is rejected", func(t *testing.T) {
		raw := payload(map[string]any{"repository": map[string]any{"full_name": "noslash"}})
		if _, err := ParseEvent(raw); err == nil || !strings.Contains(err.Error(), "owner/repo") {
			t.Fatalf("want an owner/repo error, got: %v", err)
		}
	})

	t.Run("incomplete split fields are rejected", func(t *testing.T) {
		raw := payload(map[string]any{"repository": map[string]any{"path": "widget"}})
		if _, err := ParseEvent(raw); err == nil {
			t.Fatal("want an error when the namespace cannot be derived")
		}
	})
}

// A sha padded with whitespace is the shape a hand-copied payload takes, and
// passing it through would produce a git fetch that fails obscurely later.
func TestParseEventTrimsWhitespace(t *testing.T) {
	raw := []byte(`{"action":" open ","pull_request":{"number":1,"head":{"sha":" a1 "},"base":{"sha":" b1 "}},"repository":{"full_name":" acme/widget "}}`)
	job, err := ParseEvent(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if job.HeadSha != "a1" || job.BaseSha != "b1" || job.Owner != "acme" || job.Repo != "widget" {
		t.Fatalf("whitespace not trimmed: %+v", job)
	}
}

func TestReadBodyEnforcesTheSizeCap(t *testing.T) {
	huge := strings.Repeat("x", maxPayloadBytes+1)
	req, err := http.NewRequest(http.MethodPost, "/webhook", strings.NewReader(huge))
	if err != nil {
		t.Fatal(err)
	}
	recorder := &discardRecorder{header: http.Header{}}
	if _, err := readBody(recorder, req); err == nil {
		t.Fatal("want the body cap to reject an oversized payload")
	}
}

func TestReadBodyPassesThroughSmallPayloads(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "/webhook", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	recorder := &discardRecorder{header: http.Header{}}
	body, err := readBody(recorder, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("got %q", body)
	}
}

// discardRecorder is the minimum http.ResponseWriter readBody needs; it never
// writes anything a test inspects, only satisfies the signature.
type discardRecorder struct {
	header http.Header
}

func (d *discardRecorder) Header() http.Header         { return d.header }
func (d *discardRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (d *discardRecorder) WriteHeader(int)             {}
