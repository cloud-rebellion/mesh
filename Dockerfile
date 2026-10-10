# Build + run the Mesh open core: the `mesh` CLI (index, retrieve, MCP server, web app).
#
# This is the PUBLIC image, and it is the one the fair-code mirror ships, so it must build
# standalone from that mirror alone. It may therefore never name anything the mirror
# strips (the pro hub command, its package, its entrypoint or its compose file): a release
# gate refuses to publish a build file that does, matching on the literal path, comments
# included. The pro hub image lives beside the private hub deployment files, not here.
#
# The usual install path is still `go install github.com/bright-interaction/mesh/cmd/mesh@latest`
# or `make install`; this exists for people who would rather run Mesh in a container.

# ---------- build stage ----------
FROM golang:1.26.9-alpine@sha256:cdfd4fe2da6b225d8b40c6b7a105736e548e83ff56d5d8f9394446eeb5eb84e0 AS builder
RUN apk add --no-cache git
WORKDIR /app
# The exact commit and release semver are stamped separately: diagnostics keep the SHA,
# while update checks compare the semver read from VERSION.
ARG MESH_GIT_SHA="dev"
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    MESH_RELEASE_VERSION="$(cat VERSION)" && \
    CGO_ENABLED=0 GOOS=linux go build \
      -ldflags "-X github.com/bright-interaction/mesh/internal/buildinfo.Version=$MESH_GIT_SHA -X github.com/bright-interaction/mesh/internal/buildinfo.ReleaseVersion=$MESH_RELEASE_VERSION" \
      -o /mesh ./cmd/mesh

# ---------- runtime stage ----------
FROM alpine:3.21
# git: `mesh` reads git metadata when a vault is a repo. wget: healthchecks for `mesh ui`.
RUN apk add --no-cache ca-certificates git wget && \
    adduser -D -u 1000 mesh && \
    mkdir -p /vault && chown -R mesh:mesh /vault

COPY --from=builder /mesh /usr/local/bin/mesh

USER mesh
WORKDIR /vault
# Mount your vault at /vault. Default command serves the web app on all interfaces, which
# is fail-closed: binding beyond loopback REQUIRES a token, so set MESH_UI_TOKEN.
#   docker run --rm -p 7474:7474 -e MESH_UI_TOKEN=... -v "$PWD:/vault" mesh
EXPOSE 7474
ENTRYPOINT ["mesh"]
CMD ["ui", "/vault", "--addr", "0.0.0.0:7474"]
