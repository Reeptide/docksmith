.PHONY: build test clean setup demo

BINARY := docksmith

# CGO_ENABLED=0 keeps the binary static. archive/tar pulls in os/user, which
# pulls in runtime/cgo, and a static binary is wanted for a container runtime.
build:
	CGO_ENABLED=0 go build -o $(BINARY) .

# Unit tests need no privileges — run them without sudo.
test:
	go test ./...

clean:
	rm -f $(BINARY)
	rm -rf ~/.docksmith

setup: build
	./setup/import-base-images.sh

# End-to-end verification. Requires root: namespaces, pivot_root and netlink.
demo: build
	./scripts/demo.sh
