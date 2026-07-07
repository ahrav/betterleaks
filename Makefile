.PHONY: test test-cover failfast profile clean format build

PKG=github.com/betterleaks/betterleaks
VERSION := $(shell git fetch --tags 2>/dev/null; git describe --tags --abbrev=0 2>/dev/null || echo "dev")
LDFLAGS=-ldflags "-X=github.com/betterleaks/betterleaks/version.Version=$(VERSION)"
COVER=--cover --coverprofile=cover.out

test-cover:
	go test -v ./... --race $(COVER) $(PKG)
	go tool cover -html=cover.out

format:
	go fmt ./...

test: config/betterleaks.toml format
	go test -v --race ./... $(PKG)

failfast: format
	go test -failfast ./...

# Native RE2 (cgo) is ~2x faster end-to-end than the default wazero engine.
# Requires libre2 headers (re2-devel / libre2-dev). Use `make build-portable`
# for a CGO-free static binary.
build:
	CGO_ENABLED=1 go build -tags re2_cgo $(LDFLAGS) -o betterleaks .

build-portable:
	CGO_ENABLED=0 go build $(LDFLAGS) -o betterleaks .

lint:
	golangci-lint run

clean:
	rm -rf profile
	find . -type f -name '*.got.*' -delete
	find . -type f -name '*.out' -delete

profile: build
	./scripts/profile.sh './betterleaks' '.'

config/betterleaks.toml: $(wildcard cmd/generate/config/**/*)
	go generate ./...
