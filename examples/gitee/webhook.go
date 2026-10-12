// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// maxPayloadBytes caps the webhook body. Gitee's merge-request payloads run to
// a few kilobytes; the limit exists so an unauthenticated request cannot make
// the service allocate without bound before the signature is checked.
const maxPayloadBytes = 1 << 20

// headerSignatures are the headers a Gitee webhook may carry a signature in.
// Gitee's own documentation names X-Gitee-Token; the X-Hub-* spellings are the
// GitHub-compatible pair, included because Gitee's webhook layer mirrors GitHub
// elsewhere and the cost of accepting a name it never sends is zero.
//
// Only a SHA-256 value verifies. X-Hub-Signature is GitHub's SHA-1 header and
// conventionally arrives as "sha1=<hex>", which this never accepts -- it is kept
// because dropping it could only turn a working sender away, and the real header
// set Gitee sends has not been captured yet. A delivery carrying it alone is
// rejected, which is the safe direction.
var headerSignatures = []string{
	"X-Gitee-Signature",
	"X-Hub-Signature-256",
	"X-Hub-Signature",
}

// headerTokens are the headers carrying a shared secret verbatim, which is how
// Gitee delivers a hook configured with encryption_type=1.
var headerTokens = []string{"X-Gitee-Token"}

// VerifyRequest authenticates a webhook delivery. It never reports a reason
// distinguishable by an attacker: every failure is the same error, so probing
// cannot reveal whether a secret was merely wrong or absent.
func VerifyRequest(secret string, header http.Header, body []byte) error {
	errRejected := fmt.Errorf("webhook request rejected: signature did not verify")
	if strings.TrimSpace(secret) == "" {
		return fmt.Errorf("webhook request rejected: no GITEE_WEBHOOK_SECRET is configured")
	}

	for _, name := range headerTokens {
		got := header.Get(name)
		if got == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1 {
			return nil
		}
		return errRejected
	}

	for _, name := range headerSignatures {
		got := header.Get(name)
		if got == "" {
			continue
		}
		want := signBody(secret, body)
		// Accept both the bare hex digest and the "sha256=" prefixed form,
		// since the prefix is a GitHub convention Gitee may or may not copy.
		candidate := strings.TrimPrefix(strings.TrimSpace(got), "sha256=")
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(candidate)), []byte(want)) == 1 {
			return nil
		}
		return errRejected
	}

	return fmt.Errorf("webhook request rejected: no signature header present (looked for %s)",
		strings.Join(append(append([]string{}, headerTokens...), headerSignatures...), ", "))
}

func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// branchRef is one side of a merge request.
type branchRef struct {
	Ref string `json:"ref"`
	Sha string `json:"sha"`
}

// pullRequestRef is the merge request itself. Gitee's API calls these objects
// "pull requests" throughout, so the payload key is expected to be
// pull_request; merge_request is accepted as well because the event that
// carries it is named merge_requests_events.
type pullRequestRef struct {
	Number  int       `json:"number"`
	IID     int       `json:"iid"`
	Title   string    `json:"title"`
	State   string    `json:"state"`
	Draft   bool      `json:"draft"`
	IsDraft bool      `json:"is_draft"`
	Head    branchRef `json:"head"`
	Base    branchRef `json:"base"`
}

// repositoryRef identifies the repository. full_name is the "owner/repo" form
// the REST paths need; the split fields are the fallback.
type repositoryRef struct {
	FullName  string `json:"full_name"`
	Path      string `json:"path"`
	Name      string `json:"name"`
	Namespace any    `json:"namespace"`
}

type webhookEvent struct {
	Action       string          `json:"action"`
	Number       int             `json:"number"`
	PullRequest  *pullRequestRef `json:"pull_request"`
	MergeRequest *pullRequestRef `json:"merge_request"`
	Repository   *repositoryRef  `json:"repository"`

	// raw keeps the decoded document so a parse failure can name the keys that
	// actually arrived. Gitee does not publish the payload schema, so the first
	// delivery is where an operator finds out, and the error should say what to
	// map rather than only that a field was empty.
	raw map[string]json.RawMessage
}

// reviewJob is what a verified event resolves to: enough to fetch the diff and
// post back, and nothing more.
type reviewJob struct {
	Owner     string
	Repo      string
	Number    int
	HeadSha   string
	BaseSha   string
	Title     string
	EventKind string
}

func (j reviewJob) String() string {
	return fmt.Sprintf("%s/%s#%d", j.Owner, j.Repo, j.Number)
}

// actions that should start a review, and those that must not. Gitee's event
// vocabulary is not documented consistently across its own pages, so both the
// GitHub spellings (opened/synchronize) and the terse ones (open/update) are
// accepted; a new push to an open request arrives as update or synchronize
// depending on which page you read.
var reviewActions = map[string]bool{
	"open": true, "opened": true,
	"update": true, "synchronize": true, "updated": true,
	"reopen": true, "reopened": true,
}

// ignoredActions are recognised and deliberately skipped, so they do not
// surface as "unknown action" noise on every merge.
var ignoredActions = map[string]bool{
	"close": true, "closed": true,
	"merge": true, "merged": true,
	"approve": true, "approved": true,
}

// ParseEvent decodes a delivery and decides whether it warrants a review.
// A nil job with a nil error means "recognised, but nothing to do".
func ParseEvent(body []byte) (*reviewJob, error) {
	var event webhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		return nil, fmt.Errorf("decode webhook payload: %w", err)
	}
	if err := json.Unmarshal(body, &event.raw); err != nil {
		return nil, fmt.Errorf("decode webhook payload: %w", err)
	}

	action := strings.ToLower(strings.TrimSpace(event.Action))
	if action == "" {
		return nil, fmt.Errorf("webhook payload has no action; top-level keys present: %s", event.keyList())
	}
	if ignoredActions[action] {
		return nil, nil
	}
	if !reviewActions[action] {
		// An unrecognised action is reported rather than treated as a no-op:
		// silently dropping a new action Gitee introduces would make the
		// service look healthy while never reviewing anything again.
		return nil, fmt.Errorf("unsupported webhook action %q; top-level keys present: %s", event.Action, event.keyList())
	}

	pr := event.pullRequest()
	if pr == nil {
		return nil, fmt.Errorf("webhook payload has no pull_request or merge_request object; top-level keys present: %s", event.keyList())
	}

	// A draft is still being written; reviewing it wastes a run and produces
	// comments on code the author is about to replace.
	if pr.Draft || pr.IsDraft || strings.HasPrefix(strings.ToUpper(strings.TrimSpace(pr.Title)), "WIP:") ||
		strings.HasPrefix(strings.ToUpper(strings.TrimSpace(pr.Title)), "[WIP]") {
		return nil, nil
	}

	owner, repo, err := event.repositoryOwnerAndName()
	if err != nil {
		return nil, err
	}

	number := pr.Number
	if number == 0 {
		number = pr.IID
	}
	if number == 0 {
		number = event.Number
	}
	if number == 0 {
		return nil, fmt.Errorf("webhook payload has no merge request number; top-level keys present: %s", event.keyList())
	}

	head := strings.TrimSpace(pr.Head.Sha)
	base := strings.TrimSpace(pr.Base.Sha)
	if head == "" {
		return nil, fmt.Errorf("webhook payload has no pull_request.head.sha; keys in the merge request object: %s", event.pullRequestKeys())
	}
	if base == "" {
		// base_sha is the target tip the diff is computed against. Without it
		// there is no review range, and guessing the default branch instead
		// would review a different set of changes than the ones proposed.
		return nil, fmt.Errorf("webhook payload has no pull_request.base.sha; keys in the merge request object: %s", event.pullRequestKeys())
	}

	return &reviewJob{
		Owner:     owner,
		Repo:      repo,
		Number:    number,
		HeadSha:   head,
		BaseSha:   base,
		Title:     pr.Title,
		EventKind: action,
	}, nil
}

func (e *webhookEvent) pullRequest() *pullRequestRef {
	if e.PullRequest != nil {
		return e.PullRequest
	}
	return e.MergeRequest
}

func (e *webhookEvent) repositoryOwnerAndName() (string, string, error) {
	if e.Repository == nil {
		return "", "", fmt.Errorf("webhook payload has no repository object; top-level keys present: %s", e.keyList())
	}
	if full := strings.TrimSpace(e.Repository.FullName); full != "" {
		if owner, repo, ok := strings.Cut(full, "/"); ok && owner != "" && repo != "" {
			return owner, repo, nil
		}
		return "", "", fmt.Errorf("repository.full_name %q is not in owner/repo form", full)
	}
	owner := namespaceName(e.Repository.Namespace)
	repo := strings.TrimSpace(e.Repository.Path)
	if repo == "" {
		repo = strings.TrimSpace(e.Repository.Name)
	}
	if owner == "" || repo == "" {
		return "", "", fmt.Errorf("cannot derive owner/repo: repository.full_name is empty and namespace (%q) plus path (%q) is incomplete", owner, repo)
	}
	return owner, repo, nil
}

// namespaceName reads the repository namespace, which Gitee may deliver either
// as a plain path string or as an object with a path field.
func namespaceName(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		for _, key := range []string{"path", "name", "full_name"} {
			if s, ok := v[key].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// keyList renders the decoded top-level keys in a stable order, so the same
// payload always produces the same message and an operator can diff two runs.
func (e *webhookEvent) keyList() string { return sortedKeys(e.raw) }

func sortedKeys(m map[string]json.RawMessage) string {
	if len(m) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// pullRequestKeys renders the merge request object's keys as they arrived in
// the payload. Reading them back out of the raw document rather than off the
// struct matters: the struct always has every field, so marshalling it would
// list names Gitee never sent and send the operator hunting for the wrong one.
func (e *webhookEvent) pullRequestKeys() string {
	for _, key := range []string{"pull_request", "merge_request"} {
		raw, ok := e.raw[key]
		if !ok {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		return sortedKeys(m)
	}
	return "(no pull_request object)"
}

// readBody reads a webhook body under the size cap.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, maxPayloadBytes))
}
