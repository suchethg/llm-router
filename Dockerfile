# One Dockerfile for every program in cmd/:
#   docker build --build-arg CMD=router   -t llm-router/router:dev .
#   docker build --build-arg CMD=mockvllm -t llm-router/mockvllm:dev .
FROM golang:1.23 AS build
WORKDIR /src
# Dependencies first, so code changes don't re-download modules.
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG CMD=router
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

# Static binary on a minimal non-root base: no shell, nothing else to patch.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
# Numeric UID (distroless "nonroot"), so Kubernetes runAsNonRoot can verify it.
USER 65532:65532
ENTRYPOINT ["/app"]
