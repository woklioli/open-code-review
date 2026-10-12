# syntax=docker/dockerfile:1
#
# Build from the repository root, since ocr is built from this repository's own
# source:
#
#   docker build -f examples/gitee/Dockerfile -t ocr-gitee .
#
# The Go image tag tracks the `go` directive in go.mod. Bump both together: a
# toolchain older than that directive makes the build download one, which only
# works while the build has network access.

# ---------------------------------------------------------------------------
# ocr, built from this repository
# ---------------------------------------------------------------------------
FROM golang:1.26.9-alpine AS build-ocr

WORKDIR /src
# The module proxy is a build argument because the default is slow or
# unreachable from some networks; pass --build-arg GOPROXY=... to override it.
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
# Only cmd/ and internal/ are copied, because that is all the ocr binary needs:
# every file it embeds lives inside those two trees. Copying the whole
# repository would drag in the docs site's node_modules and the editor
# extensions for no benefit.
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/

ARG VERSION=docker
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 go build \
      -ldflags "-s -w -X main.Version=${VERSION} -X main.GitCommit=${GIT_COMMIT} -X main.BuildDate=${BUILD_DATE}" \
      -o /out/ocr ./cmd/opencodereview

# ---------------------------------------------------------------------------
# The webhook service
# ---------------------------------------------------------------------------
FROM golang:1.26.9-alpine AS build-service

WORKDIR /src
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
COPY go.mod go.sum ./
RUN go mod download
COPY examples/gitee/ ./examples/gitee/

RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o /out/gitee-service ./examples/gitee

# ---------------------------------------------------------------------------
# Runtime
# ---------------------------------------------------------------------------
FROM alpine:3.21

# git is not optional: ocr shells out to it to resolve the reviewed range, so an
# image without it starts cleanly and then fails on every webhook. The
# certificates are for the Gitee API, the model gateway, and npm's CDN.
RUN apk add --no-cache ca-certificates git

ENV GITEE_ADDR=:8080 \
    GITEE_WORK_ROOT=/work \
    OCR_BIN=/usr/local/bin/ocr

COPY --from=build-ocr /out/ocr /usr/local/bin/ocr
COPY --from=build-service /out/gitee-service /usr/local/bin/gitee-service

# The service never needs to write outside its work root and /tmp, where it
# keeps the git credential helper. Running unprivileged means a bug in the
# review path cannot reach the rest of the host.
RUN adduser -D -u 10001 ocr && mkdir -p /work && chown ocr:ocr /work
USER ocr
WORKDIR /work

EXPOSE 8080

# A count of in-flight reviews that never falls is the signal that a review is
# wedged and Gitee is retrying a webhook that will not be answered. The port is
# taken from GITEE_ADDR rather than pinned, so moving the listener does not
# leave a container that reports itself unhealthy while serving normally.
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- "http://127.0.0.1:${GITEE_ADDR##*:}/healthz" || exit 1

ENTRYPOINT ["/usr/local/bin/gitee-service"]
