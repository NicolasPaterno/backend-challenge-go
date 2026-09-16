DATABASE_URL ?= postgres://wagering:wagering@localhost:5432/wagering?sslmode=disable
export DATABASE_URL

.PHONY: run build test test-race test-integration vet fmt migrate-up migrate-down

run:
	go run ./cmd/api

build:
	go build -o bin/api ./cmd/api
	go build -o bin/migrate ./cmd/migrate

test:
	go test ./...

test-race:
	go test -race ./...

test-integration:
	go test -race -tags=integration ./...

vet:
	go vet ./...
	go vet -tags=integration ./...

fmt:
	gofmt -w -l .

migrate-up:
	go run ./cmd/migrate up

migrate-down:
	go run ./cmd/migrate steps -1
