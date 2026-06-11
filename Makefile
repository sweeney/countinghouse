.PHONY: build test lint fmt clean deploy

BINARY := countinghouse
MAIN   := ./cmd/countinghouse

build:
	go build -o bin/$(BINARY) $(MAIN)

test:
	go test -race -count=1 ./...

lint:
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -rf bin/

DEPLOY_HOST ?= sweeney@garibaldi

deploy:
	./deploy/deploy.sh $(DEPLOY_HOST)
