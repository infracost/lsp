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

.PHONY: vscode-deps
vscode-deps: ## Install dependencies for the VS Code plugin
	cd plugins/vscode && npm ci

.PHONY: vscode-build
vscode-build: vscode-deps ## Build the VS Code plugin
	cd plugins/vscode && npm run compile

.PHONY: vscode-debug
vscode-debug: vscode-build ## Build the LSP with debug flags and run the VS Code plugin in development mode
	go build -gcflags="all=-N -l" -o bin/infracost-ls .
	cp bin/infracost-ls ~/go/bin/infracost-ls
	code --extensionDevelopmentPath=$(PWD)/plugins/vscode
	@echo "LSP running. Attach debugger with: dlv attach $$(pgrep infracost-ls)"

.PHONY: vscode-run
vscode-run: build install vscode-build ## Run the VS Code plugin in development mode
	code --extensionDevelopmentPath=$(PWD)/plugins/vscode

.PHONY: nvim-run
nvim-run: build install ## Run the Neovim plugin in development mode
	nvim -c 'source plugins/neovim/plugin/infracost.lua' $(DIR)

.PHONY: jetbrains-run
jetbrains-run: build install ## Run the JetBrains plugin in development mode
	cd plugins/jetbrains && ./gradlew buildPlugin && ./gradlew runIde

.PHONY: lint_install
lint_install: ## Install golangci-lint
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest

