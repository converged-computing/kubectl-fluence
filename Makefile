BINARY := kubectl-fluence
PKG := ./cmd/kubectl-fluence

.PHONY: build test install clean fmt vet
build:
	go build -o $(BINARY) $(PKG)
test:
	go test ./...
fmt:
	gofmt -w .
vet:
	go vet ./...
install: build
	install -m 0755 $(BINARY) $(DESTDIR)/usr/local/bin/$(BINARY)
clean:
	rm -f $(BINARY)
