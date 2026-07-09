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

# Default build is CGO-free and fully portable: regex runs on the bundled
# WASM RE2 (optimized wazero fork). Use `make build-cgo` to opt in to
# native RE2 via cgo (~1.3x faster parallel scans; requires libre2 headers
# from re2-devel / libre2-dev).
build:
	CGO_ENABLED=0 go build $(LDFLAGS) -o betterleaks .

build-cgo:
	CGO_ENABLED=1 go build -tags re2_cgo $(LDFLAGS) -o betterleaks .

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
