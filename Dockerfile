# syntax=docker/dockerfile:1

# Must match the `go` directive in go.mod (§4).
FROM golang:1.27-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

ENV CGO_ENABLED=0
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api && \
    go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate

FROM alpine:3.22 AS runtime

# wget backs the compose healthcheck; ca-certificates is for the IdP and SQS.
RUN apk add --no-cache ca-certificates wget && \
    adduser -D -u 10001 app

COPY --from=build /out/api /usr/local/bin/api
COPY --from=build /out/migrate /usr/local/bin/migrate

USER app
EXPOSE 8080

# Exec form: the process is PID 1 and receives SIGTERM directly (§4).
ENTRYPOINT ["/usr/local/bin/api"]
