.ONESHELL:
.DELETE_ON_ERROR:
export SHELL     := bash
export SHELLOPTS := pipefail:errexit
MAKEFLAGS += --warn-undefined-variables
MAKEFLAGS += --no-builtin-rule

# Adapted from https://www.thapaliya.com/en/writings/well-documented-makefiles/
.PHONY: help
help: ## Display this help
help:
	@awk 'BEGIN {FS = ": ##"; printf "Usage:\n  make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_\.\-\/%]+: ##/ { printf "  %-45s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

JSONNET_FLAGS := -J test -J test/vendor
KUBERNETES_LIB := github.com/jsonnet-libs/k8s-libsonnet/1.22

.PHONY: golden
golden: ## Recreate all golden files.
golden: test/common-test.golden

test:
	mkdir -p $@

test/jsonnetfile.json: ## Initialize the jb package manager.
test/jsonnetfile.json: | test
	if [[ ! -f $@ ]]; then cd $(@D) && jb init; fi

test/jsonnetfile.lock.json: ## Install the Kubernetes Jsonnet library needed for testing.
test/jsonnetfile.lock.json: test/jsonnetfile.json
	cd $(@D) && jb install $(KUBERNETES_LIB)@main

test/vendor: ## Update Jsonnet vendoring to match the lock file contents.
test/vendor: test/jsonnetfile.lock.json test/jsonnetfile.json
	cd $(@D) && jb update
	touch $@

test/k.libsonnet: ## Create the Kubernetes Jsonnet library file that is assumed to exist by other Jsonnet libraries.
test/k.libsonnet: test/jsonnetfile.json
	echo "import '$(KUBERNETES_LIB)/main.libsonnet'" > $@

test/%-test.golden: ## Produce golden files that can be used for comparison.
test/%-test.golden: %-test.jsonnet %.jsonnet %.libsonnet test/k.libsonnet test/vendor
	mkdir -p $@
	mv $@{,.old}
	trap "if [[ -d $@ ]]; then rm -rf $@.old; else mv $@{.old,}; fi" EXIT
	files="$$(jsonnet $(JSONNET_FLAGS) -Se "std.join('\n', std.objectFields(import '$<'))")"
	for f in $${files}; do
		mkdir -p "$@/$$(dirname $$f)"
		jsonnet $(JSONNET_FLAGS) -e "(import '$<')['$$f']" &> "$@/$$f" || true
	done
