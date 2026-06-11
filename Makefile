.PHONY: build test lint lint-spec fmt clean deploy

BINARY := countinghouse
MAIN   := ./cmd/countinghouse

build:
	go build -o bin/$(BINARY) $(MAIN)

test:
	go test -race -count=1 ./...

lint:
	go vet ./...

lint-spec:
	npx --yes @stoplight/spectral-cli@latest lint internal/httpapi/openapi.yaml

fmt:
	gofmt -w .

clean:
	rm -rf bin/

DEPLOY_HOST ?= sweeney@garibaldi

deploy:
	./deploy/deploy.sh $(DEPLOY_HOST)
