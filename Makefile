LDFLAGS := -s -w
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

.PHONY: build test vet dist clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/backupproof ./cmd/backupproof

test:
	go test ./...

vet:
	go vet ./...

# dist builds every platform into dist/downloads. Put that folder next to the
# server binary (or pass --downloads) so the dashboard's one-line install
# commands can hand the right agent to each computer.
dist:
	@mkdir -p dist/downloads
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="$(LDFLAGS)" \
			-o dist/downloads/backupproof-$$os-$$arch$$ext ./cmd/backupproof || exit 1; \
	done
	cd dist/downloads && sha256sum backupproof-* > SHA256SUMS

clean:
	rm -rf bin dist
