FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY cmd/kakt/ ./cmd/kakt/
COPY internal/kakt-cli/ ./internal/kakt-cli/
COPY internal/assets/ ./internal/assets/

RUN CGO_ENABLED=0 GOOS=linux go build -mod=mod -o kakt ./cmd/kakt

FROM alpine:latest

RUN apk --no-cache add ca-certificates kubectl

WORKDIR /root/

COPY --from=builder /app/kakt .

CMD ["./kakt"]
