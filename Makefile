.PHONY: help build test vet check initramfs kvm-test

help: ## Show this help
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-10s %s\n", $$1, $$2}'

build: ## Build every binary into bin/
	go build -o bin/ ./cmd/...

test: ## Unit tests (no root, KVM or network)
	go test ./...

vet: ## go vet
	go vet ./...

check: vet test ## What CI runs

initramfs: ## The agent's initramfs, dist/initramfs.cpio.gz (scripts/build-initramfs.sh)
	./scripts/build-initramfs.sh

kvm-test: ## The nodelet on real Firecracker, in a container with /dev/kvm (scripts/kvm-test.sh)
	./scripts/kvm-test.sh
