OWNER   ?= raspbeguy
NAME    ?= matrix
VERSION ?= 0.1.0
OS_ARCH ?= $(shell go env GOOS)_$(shell go env GOARCH)

BINARY  := terraform-provider-$(NAME)
TF_INSTALL_DIR := $(HOME)/.terraform.d/plugins/registry.terraform.io/$(OWNER)/$(NAME)/$(VERSION)/$(OS_ARCH)
TOFU_INSTALL_DIR := $(HOME)/.terraform.d/plugins/registry.opentofu.org/$(OWNER)/$(NAME)/$(VERSION)/$(OS_ARCH)

.PHONY: build install test testacc vet tidy clean docs

build:
	go build -o $(BINARY) .

install: build
	mkdir -p $(TF_INSTALL_DIR) $(TOFU_INSTALL_DIR)
	cp $(BINARY) $(TF_INSTALL_DIR)/
	mv $(BINARY) $(TOFU_INSTALL_DIR)/

test:
	go test -v ./...

testacc:
	TF_ACC=1 go test -v ./internal/provider/... -run TestAcc -timeout 30m

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -f $(BINARY)

docs:
	go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs generate \
		--provider-name $(NAME) \
		--rendered-provider-name "Matrix" \
		--examples-dir examples \
		--rendered-website-dir docs
