# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/teams-a2a-gateway ./cmd/teams-a2a-gateway

# Static, shell-less, non-root runtime image.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/teams-a2a-gateway /usr/local/bin/teams-a2a-gateway
USER 65532:65532
EXPOSE 3978 9090
ENTRYPOINT ["/usr/local/bin/teams-a2a-gateway"]
CMD ["-config", "/etc/teams-a2a/config.yaml"]
