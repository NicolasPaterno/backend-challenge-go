DATABASE_URL ?= postgres://wagering:wagering@localhost:5432/wagering?sslmode=disable
export DATABASE_URL

.PHONY: run build test test-race test-integration test-system vet fmt migrate-up migrate-down

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

# Three real processes against shared containers (§8, §13.4); slower, so it is
# not part of test-integration.
test-system:
	go test -race -tags='integration,system' -timeout 40m ./test/system/...

vet:
	go vet ./...
	go vet -tags=integration ./...
	go vet -tags='integration,system' ./...

fmt:
	gofmt -w -l .

migrate-up:
	go run ./cmd/migrate up

migrate-down:
	go run ./cmd/migrate steps -1
