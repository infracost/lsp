default: help

.PHONY: help
help: ## Show this help.
	@fgrep -h "##" $(MAKEFILE_LIST)  | fgrep -v fgrep | sed -e 's/:.*##/:##/' | awk -F':##' '{printf "%-12s %s\n",$$1, $$2}'

.PHONY: build
build: ## Build the LSP binary
	go build -o bin/infracost-ls .

.PHONY: lint
lint: lint_install ## Run linting operations
	golangci-lint run ./...

.PHONY: fmt
fmt: ## Format the code
	@gofmt -w .

.PHONY: install
install: build ## Install the LSP binary to the Go bin directory
	cp bin/infracost-ls ~/go/bin/infracost-ls

.PHONY: clean
clean: ## Clean build artifacts
	rm -rf bin/

.PHONY: nvim-run
nvim-run: build install ## Run Neovim with the LSP plugin loaded
	nvim -c 'source plugins/neovim/plugin/infracost.lua' $(DIR)

.PHONY: lint_install
lint_install: ## Install golangci-lint
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
