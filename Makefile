BINARY := logdepot
PKG    := github.com/logdepot/server
GOFLAGS := -trimpath
LDFLAGS := -s -w

.PHONY: build run test test-unit test-integration test-e2e test-all clean fmt lint

build:
	go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY) .

run: build
	./$(BINARY)

# All Go tests (unit + integration).
test:
	go test -race -count=1 ./...

# Short unit tests only (skip integration tests that need >5s).
test-unit:
	go test -race -count=1 -short ./...

# Verbose Go tests with coverage.
test-cover:
	go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out
	@echo ""
	@echo "For HTML report: go tool cover -html=coverage.out"

# Python end-to-end tests (requires built binary + Python deps).
test-e2e: build
	cd e2e && pip install -q -r requirements.txt && \
		LOGDEPOT_BINARY=../$(BINARY) pytest -v --timeout=60

# Run everything.
test-all: test test-e2e

fmt:
	gofmt -w .

lint:
	golangci-lint run ./...

clean:
	rm -f $(BINARY) $(BINARY)-linux-amd64 coverage.out

# Cross-compile for linux/amd64 (typical server target).
build-linux:
	GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY)-linux-amd64 .

# Build a minimal Docker image.
docker:
	docker build -t $(BINARY):latest .
