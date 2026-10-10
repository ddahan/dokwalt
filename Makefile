VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/ddahan/dokwalt/internal/cli.Build=$(VERSION)
GO      := CGO_ENABLED=0 go

.PHONY: build dist test e2e lint clean docs-site

# CLI for this machine + server binaries next to it (used by `server init`).
build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/dokwalt ./cmd/dokwalt
	GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/dokwalt_linux_amd64 ./cmd/dokwalt
	GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/dokwalt_linux_arm64 ./cmd/dokwalt

# Release artifacts + checksums.
dist:
	rm -rf dist && mkdir -p dist
	for p in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64; do \
		os=$${p%/*}; arch=$${p#*/}; \
		GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/dokwalt_$${os}_$${arch} ./cmd/dokwalt || exit 1; \
	done
	cd dist && shasum -a 256 dokwalt_* > checksums.txt

test:
	go test ./...

# Website docs (site/docs) from internal/docs/topics, for the latest release tag.
# `make test` fails while they are out of date.
docs-site:
	go run ./internal/docs/sitegen -out site/docs

lint:
	go vet ./...
	test -z "$$(gofmt -l .)"

# Full end-to-end run against a throwaway Linux server in Docker.
e2e: build
	./test/e2e/run.sh

clean:
	rm -rf bin dist
