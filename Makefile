BIN := gh-actions-archive-cache

.PHONY: build fmt fmt-check vet test test-unit test-integration

build:
	go build -o $(BIN) ./cmd/gh-actions-archive-cache

fmt:
	gofmt -w .

fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not gofmt-clean:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

vet:
	go vet ./...

test:
	go test -race -count=1 ./...

test-unit:
	go test -race -count=1 ./internal/... ./cmd/...

test-integration:
	go test -race -count=1 ./test/integration/...
