FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

# kamarkat-mcp (ADR-116): the command, the server package and the JWKS
# validator it shares with the auth proxy.
COPY cmd/mcp-server/ ./cmd/mcp-server/
COPY internal/platform/boxmcp/ ./internal/platform/boxmcp/
COPY internal/auth-proxy/ ./internal/auth-proxy/

ARG VERSION=development
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} go build -mod=mod \
    -ldflags "-X github.com/kamarkat/kamarkat/internal/platform/boxmcp.Version=${VERSION}" \
    -o mcp-server ./cmd/mcp-server

FROM alpine:3.21

RUN apk --no-cache add ca-certificates

COPY --from=builder /app/mcp-server /usr/local/bin/mcp-server

USER 65532:65532
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/mcp-server"]
