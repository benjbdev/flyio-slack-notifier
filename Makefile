GO ?= /usr/local/go/bin/go
BIN := notifier
CONFIG ?= config.yaml

.PHONY: build dev run test test-integration tidy fmt vet clean

build:
	$(GO) build -o $(BIN) ./cmd/notifier

run: build
	./$(BIN) --config $(CONFIG)

dev:
	$(GO) run ./cmd/notifier --config $(CONFIG)

test:
	$(GO) test ./...

test-integration:
	$(GO) test -tags integration ./...

tidy:
	$(GO) mod tidy

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

clean:
	rm -f $(BIN) notifier.db
