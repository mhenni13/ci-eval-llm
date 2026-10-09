BIN := ci-eval-llm

.PHONY: build test race vet demo install clean

build:
	go build -o $(BIN) ./cmd/ci-eval-llm

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

demo: build
	./$(BIN) run examples/tests.yaml --workers 10 || true
	./$(BIN) report

install:
	go install ./cmd/ci-eval-llm

clean:
	rm -f $(BIN)
	rm -rf .ci-eval-llm
