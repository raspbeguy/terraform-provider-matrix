OWNER   ?= raspbeguy
NAME    ?= matrix
VERSION ?= 0.1.0
OS_ARCH ?= $(shell go env GOOS)_$(shell go env GOARCH)

# The acceptance harness reattaches the in-process provider under this address,
# which has to match the source the test configs declare, and it drives a real
# CLI binary. Override all three to run the suite against Terraform instead.
TF_ACC_PROVIDER_HOST      ?= registry.opentofu.org
TF_ACC_PROVIDER_NAMESPACE ?= raspbeguy
TF_ACC_TERRAFORM_PATH     ?= $(shell command -v tofu 2>/dev/null)

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

# Needs a homeserver it may pollute, and a persistent one is supported: the
# aliases each test creates are randomised and cleaned up, so runs do not
# collide. Each run does leave one room per test behind. Matrix has no way to
# delete a room, and destroy only makes the account leave, so purging them needs
# the admin API.
testacc:
	TF_ACC=1 \
	TF_ACC_PROVIDER_HOST=$(TF_ACC_PROVIDER_HOST) \
	TF_ACC_PROVIDER_NAMESPACE=$(TF_ACC_PROVIDER_NAMESPACE) \
	TF_ACC_TERRAFORM_PATH=$(TF_ACC_TERRAFORM_PATH) \
	go test -v ./internal/provider/... -run TestAcc -timeout 30m

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
