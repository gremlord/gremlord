BINARY := gremlord
VERSION ?= dev
LDFLAGS := -ldflags "-X github.com/gremlord/gremlord/internal/router.Version=$(VERSION)"

.PHONY: build test vet install clean

build:
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

install: build
	install -m 0755 $(BINARY) /usr/local/bin/$(BINARY)

clean:
	rm -f $(BINARY)
