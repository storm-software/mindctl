# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
  -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
  -o /out/mindctl ./cmd/mindctl

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="mindctl" \
  org.opencontainers.image.description="An LLM router that uses built-in logic and system 1 decision models to intelligently route requests to the appropriate LLM based on the input prompt" \
  org.opencontainers.image.licenses="Apache-2.0" \
  org.opencontainers.image.vendor="Storm Software" \
  org.opencontainers.image.url="https://stormsoftware.com/projects/mindctl" \
  org.opencontainers.image.documentation="https://stormsoftware.com/projects/mindctl"
COPY --from=build /out/mindctl /usr/local/bin/mindctl
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/mindctl"]
CMD ["--config", "/etc/mindctl/config.yaml"]
