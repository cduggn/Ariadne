# The gateway image is a static Go binary on distroless, with no shell and no package manager.
#   make gateway-image      # builds linux/amd64 + linux/arm64, pushes, pins the digest in deploy/k8s/gateway.yaml
# The builder runs on the host's own architecture and cross-compiles, so neither platform needs emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.0@sha256:4013ae0f9e7994f8535c58c811f8f863fbed38b72e0d51e6592156f758d66146 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
ENV GOWORK=off
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags=-s -o /out/gateway ./cmd/gateway

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/gateway /gateway
USER 65532:65532
EXPOSE 8000
ENTRYPOINT ["/gateway"]
