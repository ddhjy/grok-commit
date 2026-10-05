.PHONY: build test check package
VERSION ?= dev

build:
	go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o dist/grok-commit ./cmd/grok-commit

test:
	go test -race -timeout 120s ./...

check:
	go vet ./...
	@test -z "$$(gofmt -l cmd internal)"

package:
	go run ./cmd/package -version $(VERSION)
