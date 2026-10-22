.PHONY: build test vet e2e install clean

build:
	go build -o bin/agentdeck ./cmd/agentdeck

test:
	go test ./...

vet:
	go vet ./...

e2e: build
	python3 scripts/e2e.py

install: build
	install -Dm755 bin/agentdeck $(HOME)/.local/bin/agentdeck

clean:
	rm -rf bin
