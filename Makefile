# Makefile

SERVICE := tilesservice

REGISTRY := ghcr.io/swayrider
IMAGE    := $(REGISTRY)/$(SERVICE)

VERSION_TAG    := $(shell git tag --points-at HEAD 2>/dev/null | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$$' | sort -V | tail -1)
LAST_VERSION   := $(shell git describe --tags --match 'v[0-9]*.[0-9]*.[0-9]*' --abbrev=0 2>/dev/null || echo v0.0.0)
CURRENT_BRANCH := $(shell git symbolic-ref --short HEAD 2>/dev/null)
DATE_TAG       := $(shell date +%Y%m%d)
SHORT_SHA      := $(shell git rev-parse --short HEAD 2>/dev/null)
SAFE_BRANCH    := $(shell echo "$(CURRENT_BRANCH)" | sed 's|/|-|g; s|[^a-zA-Z0-9-]|-|g')

FORCE_DEV_LATEST ?=

ifneq ($(VERSION_TAG),)
  BASE_TAG     := $(VERSION_TAG)
  FLOATING_TAG := latest
  ifeq ($(FORCE_DEV_LATEST),1)
    EXTRA_TAG := dev-latest
  else
    EXTRA_TAG :=
  endif
else ifeq ($(CURRENT_BRANCH),main)
  BASE_TAG     := $(LAST_VERSION)-$(DATE_TAG)-dev
  FLOATING_TAG := dev-latest
  EXTRA_TAG    :=
else ifneq ($(CURRENT_BRANCH),)
  BASE_TAG     := $(LAST_VERSION)-$(SAFE_BRANCH)
  FLOATING_TAG :=
  EXTRA_TAG    :=
else
  BASE_TAG     := $(LAST_VERSION)-$(SHORT_SHA)
  FLOATING_TAG :=
  EXTRA_TAG    :=
endif

# Non-release builds get an incrementing build number (-b<N>) derived from the
# registry's existing tags, so repeated builds don't overwrite each other.
# Release builds (version tag on HEAD) stay immutable — no build number.
ifneq ($(VERSION_TAG),)
  # release build: no build number
else
  BUILD_NUMBER := $(shell \
    TOKEN=$$(curl -s --max-time 15 "https://ghcr.io/token?scope=repository:swayrider/$(SERVICE):pull&service=ghcr.io" | sed -E 's/.*"token":"([^"]+)".*/\1/'); \
    if [ -z "$$TOKEN" ]; then echo ""; exit 1; fi; \
    TAG_LIST=$$(curl -s --max-time 15 -H "Authorization: Bearer $$TOKEN" "https://ghcr.io/v2/swayrider/$(SERVICE)/tags/list"); \
    if [ -z "$$TAG_LIST" ]; then echo ""; exit 1; fi; \
    LAST=$$(echo "$$TAG_LIST" | grep -oE '$(BASE_TAG)-b[0-9]+' | grep -oE '[0-9]+$$' | sort -n | tail -1); \
    if [ -z "$$LAST" ]; then echo 1; else echo $$(( $$LAST + 1 )); fi \
  )
  ifeq ($(BUILD_NUMBER),)
    $(error Could not determine build number for $(IMAGE):$(BASE_TAG) — aborting (registry unreachable?))
  endif
  BASE_TAG := $(BASE_TAG)-b$(BUILD_NUMBER)
endif

TAGS := -t $(IMAGE):$(BASE_TAG)
ifneq ($(FLOATING_TAG),)
  TAGS := $(TAGS) -t $(IMAGE):$(FLOATING_TAG)
endif
ifneq ($(EXTRA_TAG),)
  TAGS := $(TAGS) -t $(IMAGE):$(EXTRA_TAG)
endif

.PHONY: container-build

all: container-build

container-build:
	@echo "Building $(IMAGE):$(BASE_TAG)$(if $(FLOATING_TAG), [+$(FLOATING_TAG)])$(if $(EXTRA_TAG), [+$(EXTRA_TAG)])"
	docker buildx build \
		-f Dockerfile \
		--network=host \
		--platform linux/amd64,linux/arm64 \
		$(TAGS) \
		--push .
	@echo "Done."
