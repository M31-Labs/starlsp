# starlsp
#
# The default target builds and tests everything. `make install` puts both
# halves — the server and the VS Code extension — where an editor will find
# them, in one command, because a two-step install is a two-step install to get
# wrong.

SHELL := bash
.DEFAULT_GOAL := all

VERSION  := $(shell sed -n 's/^const Version = "\(.*\)"$$/\1/p' server.go)
EXT_DIR  := editors/vscode
VSIX     := $(EXT_DIR)/starlsp-$(VERSION).vsix

# The subset tags embed only the Starlark grammar, which makes the binary much
# smaller. The README has the sizes.
LEAN_TAGS := grammar_subset,grammar_subset_starlark

.PHONY: all
all: test build ## build and test everything

.PHONY: build
build: ## build the server into ./bin
	@mkdir -p bin
	go build -tags '$(LEAN_TAGS)' -o bin/starlsp ./cmd/starlsp
	@echo "built bin/starlsp $$(./bin/starlsp --version | awk '{print $$2}') ($$(du -h bin/starlsp | cut -f1))"

.PHONY: test
test: ## run the Go suite
	go test ./...

.PHONY: race
race: ## run the Go suite under the race detector
	go test -race ./...

.PHONY: corpus
corpus: ## check against real Bazel and Tilt repositories
	@mkdir -p .corpus
	@[ -d .corpus/rules_go ]      || git clone --depth 1 -q https://github.com/bazelbuild/rules_go .corpus/rules_go
	@[ -d .corpus/bazel-skylib ]  || git clone --depth 1 -q https://github.com/bazelbuild/bazel-skylib .corpus/bazel-skylib
	@[ -d .corpus/tilt ]          || git clone --depth 1 -q https://github.com/tilt-dev/tilt .corpus/tilt
	GTS_STARLARK_CORPUS=$$PWD/.corpus go test -run 'TestCorpus|TestRealWorld' -timeout 900s ./...

.PHONY: install
install: install-server install-extension ## install the server and the VS Code extension

.PHONY: install-server
install-server: ## go install the server
	go install -tags '$(LEAN_TAGS)' ./cmd/starlsp
	@echo "installed $$(command -v starlsp) $$(starlsp --version | awk '{print $$2}')"

.PHONY: vsix
vsix: ## build the VS Code extension package
	cd $(EXT_DIR) && npm install --silent --no-audit --no-fund
	cd $(EXT_DIR) && npm run build:prod
	cd $(EXT_DIR) && rm -f *.vsix && npx --yes @vscode/vsce@latest package --allow-missing-repository
	@echo "packaged $(VSIX)"

.PHONY: install-extension
install-extension: vsix ## build and install the VS Code extension
	./scripts/install-vscode-extension.sh $(VSIX)

.PHONY: test-extension
test-extension: build vsix ## run the extension's bundle and protocol suites
	cd $(EXT_DIR) && STARLSP_PATH=$(PWD)/bin/starlsp npm test

.PHONY: uninstall-extension
uninstall-extension: ## remove the extension from VS Code
	code --uninstall-extension m31labs.starlsp || true

.PHONY: clean
clean: ## remove build output
	rm -rf bin $(EXT_DIR)/dist $(EXT_DIR)/*.vsix

.PHONY: version
version: ## print the version every artifact should carry
	@echo $(VERSION)

.PHONY: release
release: ## check that everything is consistent and green, then print the tag command
	@test -z "$$(git status --porcelain)" || { echo "working tree is dirty"; exit 1; }
	$(MAKE) test
	$(MAKE) race
	$(MAKE) corpus
	$(MAKE) test-extension
	@echo
	@echo "all green at v$(VERSION). To release:"
	@echo "  git tag -a v$(VERSION) -m 'starlsp v$(VERSION)' && git push origin v$(VERSION)"

.PHONY: help
help: ## list the targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
