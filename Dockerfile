# syntax=docker/dockerfile:1.7
# One image recipe for every service: `--build-arg SERVICE=<name>` selects ./cmd/<name>.
ARG GO_VERSION=1.26

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG SERVICE
ARG VERSION=dev
ARG COMMIT=unknown
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,source=go.mod,target=go.mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    go mod download
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=bind,target=. \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags="-s -w -X github.com/smhunt/sumpnet/internal/platform.Version=${VERSION} -X github.com/smhunt/sumpnet/internal/platform.Commit=${COMMIT}" \
      -o /out/svc ./cmd/${SERVICE}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/svc /svc
EXPOSE 8080
# HEALTHCHECK is defined in compose (`/svc healthcheck`) so ECS can define its own later.
ENTRYPOINT ["/svc"]
