# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/teams-a2a-gateway ./cmd/teams-a2a-gateway

# Static, shell-less, non-root runtime image.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/teams-a2a-gateway /usr/local/bin/teams-a2a-gateway
USER 65532:65532
EXPOSE 3978 9090
ENTRYPOINT ["/usr/local/bin/teams-a2a-gateway"]
CMD ["-config", "/etc/teams-a2a/config.yaml"]
