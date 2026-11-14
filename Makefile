BINARY := bin/leigod

.PHONY: all build test vet clean install uninstall purge status

all: build

build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/leigod

test:
	go test -v -race ./...

vet:
	go vet ./...

clean:
	rm -rf bin

install: build
	sudo ./$(BINARY) install

uninstall: build
	sudo ./$(BINARY) uninstall

purge: build
	sudo ./$(BINARY) uninstall --purge

status: build
	./$(BINARY) status
