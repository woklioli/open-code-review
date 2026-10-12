# Gitee webhook service

Reviews a Gitee merge request automatically and posts the findings back to it.

Gitee has no equivalent of GitHub Actions' `pull_request_target`, so there is no
runtime to hang a review off. This example is therefore a small long-running
service rather than a pipeline script: it receives Gitee's webhook, fetches the
two commits the merge request spans, runs `ocr review` over that range, and
posts what comes back as line comments plus one sticky summary comment.

It is a single Go binary built from this repository, so the feed it reviews with
is the `ocr` in this same tree.

## Contents

| File | Purpose |
| --- | --- |
| `main.go` | HTTP entry point, routing, graceful shutdown |
| `config.go` | configuration loaded from the environment and an optional `.env` |
| `webhook.go` | payload parsing and webhook authentication |
| `queue.go` | per-merge-request serialisation and per-revision deduplication |
| `runner.go` | `git` checkout of the reviewed range, and the `ocr` subprocess |
| `gitee.go` | Gitee REST client with rate-limit backoff |
| `diffpos.go` | patch to diff-position conversion |
| `poster.go` | findings to line comments and the sticky summary |
| `deps.go` | lock-file parsing: which version of which package a revision pins |
| `candidates.go` | which findings rest on a dependency's API |
| `evidence.go` | a pinned package's published type declarations |
| `llm.go` | the minimal model client the verification round uses |
| `verify.go` | the verification round itself |
| `dryrun.go` | a client that logs writes instead of sending them |

## Running it

Build from the repository root, because the image compiles `ocr` from this
repository's source:

```sh
docker build -f examples/gitee/Dockerfile -t ocr-gitee .
```

The build resolves Go modules through `proxy.golang.org`. Where that is slow or
blocked, pass a different proxy:

```sh
docker build --build-arg GOPROXY=https://goproxy.cn,direct \
  -f examples/gitee/Dockerfile -t ocr-gitee .
```

Copy `.env.example` to `.env`, fill in the four required values (`GITEE_TOKEN`,
`GITEE_WEBHOOK_SECRET`, and the three `OCR_LLM_*` credentials), and run it:

```sh
docker run -d --name ocr-gitee \
  --env-file .env \
  -p 127.0.0.1:8080:8080 \
  ocr-gitee
```

Leave `GITEE_DRY_RUN=true` for the first run. The service then logs every
comment it would post — the path, the diff position, and the body — and posts
nothing. Check that log against a real merge request before turning it off.

Confirm the service is up:

```sh
curl -s localhost:8080/healthz
# ok in_flight=0
```

## Configuring the Gitee webhook

In the repository's settings, add a webhook with:

- **URL**: wherever the service is reachable from Gitee, e.g.
  `https://ocr.example.com/webhook`. Gitee has to be able to reach it, so
  `127.0.0.1` will not do — put a reverse proxy in front.
- **Webhook secret / signing key**: the same value as `GITEE_WEBHOOK_SECRET`.
  Use the signing-key encryption type, not the password type. The password type
  carries the secret in the URL, which puts it in every access log on the way.
- **Events**: merge request events. Push events are not used.
- **Enable SSL verification**: on, if the endpoint terminates TLS.

The service answers `POST /webhook` and ignores everything else. Recognised
events are queued and answered `202` immediately; the review runs afterwards, so
Gitee's webhook timeout is never in play.

Only `open`, `update`/`synchronize`, and `reopen` actions start a review.
Closes, merges, approvals, and drafts are accepted and dropped — they are not
errors, and returning one would make Gitee retry. An action the service has not
seen before is treated as an error and logged with the payload keys that
arrived, because a new Gitee action that silently did nothing would look
indistinguishable from an idle service.

## Configuration

Every value comes from the environment, so the same image runs anywhere and only
the injected `.env` differs. A real environment variable wins over the `.env`
file, so `docker run -e` overrides it.

| Variable | Default | Meaning |
| --- | --- | --- |
| `GITEE_API_BASE` | `https://gitee.com/api/v5` | API root; change for Gitee Enterprise. The scheme and host also become the host git fetches from, so one setting moves both |
| `GITEE_TOKEN` | — | **required**; personal access token used to post |
| `GITEE_WEBHOOK_SECRET` | — | **required**; the webhook's signing key |
| `GITEE_GIT_USERNAME` | `oauth2` | account name git authenticates with |
| `OCR_LLM_URL` | — | **required**; model endpoint |
| `OCR_LLM_TOKEN` | — | **required**; model credential |
| `OCR_LLM_MODEL` | — | **required**; model name |
| `OCR_USE_ANTHROPIC` | — | forwarded to ocr; empty means ocr's own default |
| `OCR_LLM_PROTOCOL` | — | forwarded to ocr's protocol resolution |
| `GITEE_ADDR` | `:8080` | listen address |
| `GITEE_WORK_ROOT` | the temp dir | where checkouts are written |
| `GITEE_REVIEW_TIMEOUT_MIN` | `15` | minutes handed to ocr's `--timeout` |
| `GITEE_CONCURRENCY` | `8` | reviews running at once |
| `GITEE_VERIFY_DEPS` | `true` | run the dependency verification round |
| `GITEE_DRY_RUN` | `true` | log instead of posting |

The review invocation is the same one every other integration in this repository
uses:

```
ocr review --from <base_sha> --to <head_sha> \
  --format json --audience agent \
  --timeout <GITEE_REVIEW_TIMEOUT_MIN> --concurrency <GITEE_CONCURRENCY> \
  --background <merge request title>
```

## Security

- **Nothing from the merge request is executed.** The service runs `git init`,
  `git fetch`, and `git checkout` — none of which run repository code — and then
  `ocr review` over the resulting git objects. There is deliberately no `npm ci`,
  `go mod download`, `make`, or build step anywhere: a merge request's
  `postinstall` script would run on the host that reviews it. For the same
  reason the verification round reads lock files out of the checkout rather than
  from an installed dependency tree, and reads them only when they are regular
  files, so a checked-out symlink named `package.json` cannot make it read
  something else and send that to the model as evidence.
- **Credentials never reach `argv` or `.git/config`.** Git authenticates through
  `GIT_ASKPASS`, a small helper written to the temp directory that reads the
  token from the environment. The token is passed to the Gitee API in an
  `Authorization` header, never in a query string.
- **Logs do not print secrets.** Configuration is logged redacted — tokens
  reduced to a length and a suffix — and the model credentials are handed to
  `ocr` through an allowlisted environment rather than a copy of the service's
  own, so `GITEE_TOKEN` is not passed to a subprocess with no use for it.
- **One review per merge request at a time**, and one review per revision: Gitee
  sends several deliveries for what a human considers one push, and a
  redelivered webhook is answered `202 already queued` rather than reviewing the
  same commit twice.
- **Listen internally.** The webhook endpoint authenticates its callers, but
  it is the only thing exposed; put it behind a reverse proxy rather than on a
  public interface.

## The verification round

`ocr` already re-reads its own findings against the diff and deletes the ones
the diff contradicts. That filter is deliberately conservative — its prompt says
"unverifiable is not incorrect" — so by design it keeps findings it cannot
check, and a finding about a library the diff does not contain is exactly that.
Measured on real merge requests, the surviving false positives were all of that
one kind: a claim about a framework's API, correct for the version the model had
in mind and wrong for the one the repository pins.

With `GITEE_VERIFY_DEPS=true` the service closes that gap:

1. Read the reviewed revision's lock files (`package-lock.json`,
   `pnpm-lock.yaml`, `yarn.lock`, `package.json`, `go.mod`, `requirements.txt`)
   and find which dependencies are pinned to an exact version.
2. Keep the findings that name exactly one such dependency and at least one
   identifier in backticks. A finding naming two packages is ambiguous about
   which API its claim is about, and is left alone.
3. Fetch that exact version's published type declarations from
   `cdn.jsdelivr.net` and look for the identifiers the finding names.
4. If none of them appears anywhere in the declarations, stop — the package does
   not contradict the finding and no model call is spent.
5. Otherwise ask the model whether the declarations refute the finding.

The round is built to keep findings, not to lose them. A finding is removed only
when the model says it is contradicted, gives a reason, **and that reason names
an identifier the declarations actually contain**. Every failure — no lock file,
no exact version, unsupported ecosystem, unreachable CDN, model error, cancelled
context — keeps the finding. Findings whose check could not be completed are
listed under `### Unconfirmed` in the summary rather than dropped, so silence is
never mistaken for a pass.

Most merge requests have no candidates and cost nothing extra.

## Known limitations

**The diff position is inferred, not confirmed.** Gitee documents `position`
only as "the line number in the merge request's code review diff", and reading
comments back never fills the field in — a sweep of 571 merge requests across 33
repositories returned no non-null `position`, and Gitee stores the absolute line
in `new_line` instead. The service therefore computes the position the way
GitHub's legacy position is defined (the line count below the first `@@`), and
guards against being wrong: after the first successful line comment it compares
the `new_line` Gitee reports against the line it meant to annotate, and if they
disagree it stops annotating and lists the rest of the findings in the summary
instead. A comment on the wrong line is worse than no comment, because the
reader trusts it. If you find the model is wrong for your Gitee version, the
conversion is the single function `buildLinePositions` in `diffpos.go`.

**Dependency verification covers npm only.** `go.mod` and `requirements.txt` are
parsed and their exact pins are found, but no evidence source for them is
implemented, so those findings go unchecked and are kept. `pom.xml` is not
parsed at all. Everything unparsed is kept.

**The model client implements two wire formats.** Protocol resolution mirrors
`ocr`'s own, including its default to Anthropic when nothing is configured. The
OpenAI chat-completions format is implemented as well. `openai-responses` and
`anthropic-bedrock` are not, and when one of those is configured the round
disables itself with a log line rather than failing every candidate.

**Comments are English only.** The service writes its own text — the summary
headings, the unconfirmed notes — in English, matching the repository's
source-language rule.

**Only line comments and one summary.** There is no incremental review and no
resolution of outdated comments. Each run re-reads the merge request's existing
comments: a finding already posted for this revision is skipped, and the summary
is rewritten in place rather than posted again.
