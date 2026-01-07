# A Self-Documenting Makefile: http://marmelab.com/blog/2016/02/29/auto-documented-makefile.html

# Project variables
PACKAGE = github.com/hashicorp/go-gin-gorm-opencensus
BINARY_NAME = go-gin-gorm-opencensus

# Build variables
BUILD_DIR = build
BUILD_PACKAGE = ${PACKAGE}

# Dependency versions
GOLANGCI_VERSION = 1.16.0
CURRENT_YEAR := $(shell date +%Y)
CURRENT_TIME := $(shell date +%Y%m%d_%H%M%S)

.PHONY: up
up: start .env .env.test ## Set up the development environment

.PHONY: down
down: clean ## Destroy the development environment
	docker-compose down
	rm -rf .docker/

.PHONY: reset
reset: down up ## Reset the development environment

.PHONY: clean
clean: ## Clean the working area and the project
	rm -rf bin/ ${BUILD_DIR}/

docker-compose.override.yml: ## Create docker compose override file
	cp docker-compose.override.yml.dist docker-compose.override.yml

.PHONY: start
start: docker-compose.override.yml ## Start docker development environment
	docker-compose up -d

.PHONY: stop
stop: ## Stop docker development environment
	docker-compose stop

.env: ## Create local env file
	cp .env.dist .env

.env.test: ## Create local env file for running tests
	cp .env.dist .env.test

.PHONY: run
run: TAGS += dev
run: build .env ## Build and execute a binary
	${BUILD_DIR}/${BINARY_NAME} ${ARGS}

.PHONY: build
build: ## Build a binary
	CGO_ENABLED=0 go build -tags '${TAGS}' -o ${BUILD_DIR}/${BINARY_NAME} ${BUILD_PACKAGE}

.PHONY: check
check: test lint ## Run tests and linters

.PHONY: test
test: ## Run all tests
	go test -v ./...

bin/golangci-lint: bin/golangci-lint-${GOLANGCI_VERSION}
bin/golangci-lint-${GOLANGCI_VERSION}:
	@mkdir -p bin
	@rm -rf bin/golangci-lint-*
	curl -sfL https://install.goreleaser.com/github.com/golangci/golangci-lint.sh | bash -s -- -b ./bin/ v${GOLANGCI_VERSION}
	@touch $@

.PHONY: lint
lint: bin/golangci-lint ## Run linter
	bin/golangci-lint run

.PHONY: help
.DEFAULT_GOAL := help
help:
	@grep -h -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'

# Variable outputting/exporting rules
var-%: ; @echo $($*)
varexport-%: ; @echo $*=$($*)

.PHONY: install-copywrite-ibm
install-copywrite-ibm: ## Install copywrite-ibm tool
	git clone https://github.com/hashicorp/copywrite_ibm.git /tmp/$(CURRENT_TIME)/copywrite_ibm
	make -C /tmp/$(CURRENT_TIME)/copywrite_ibm install
	cp /tmp/$(CURRENT_TIME)/copywrite_ibm/copywrite-ibm .


.PHONY: add-license-headers
add-license-headers: ## Add license headers to all source files
	./copywrite-ibm --config .copywrite.hcl headers --year1 2020 --year2 $(CURRENT_YEAR)

.PHONY: verify-license-headers
verify-license-headers: ## Verify license headers on all source files
	./copywrite-ibm --config .copywrite.hcl headers --year1 2020 --year2 $(CURRENT_YEAR) --plan
