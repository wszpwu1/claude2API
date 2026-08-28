# syntax=docker/dockerfile:1

FROM golang:1.26-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/claude2api .

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /out/claude2api /app/claude2api

ENV PORT=8080 \
    CLAUDE_BASE_URL=https://claude.ai \
    CLAUDE_TIMEZONE=Asia/Singapore \
    CLAUDE_LOCALE=en-US \
    DEFAULT_MODEL=claude-sonnet-5

EXPOSE 8080

ENTRYPOINT ["/app/claude2api"]
