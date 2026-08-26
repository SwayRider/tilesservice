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
NO_PUSH          ?=

ifneq ($(VERSION_TAG),)
  BASE_TAG     := $(VERSION_TAG)
  FLOATING_TAG := latest
else ifeq ($(CURRENT_BRANCH),main)
  BASE_TAG     := $(LAST_VERSION)-$(DATE_TAG)-dev
  FLOATING_TAG := dev-latest
else ifneq ($(CURRENT_BRANCH),)
  BASE_TAG     := $(LAST_VERSION)-$(SAFE_BRANCH)
  FLOATING_TAG :=
else
  BASE_TAG     := $(LAST_VERSION)-$(SHORT_SHA)
  FLOATING_TAG :=
endif

# FORCE_DEV_LATEST=1 adds the dev-latest floating tag on top of whatever
# BASE_TAG/FLOATING_TAG was just decided -- e.g. a release build or a
# feature-branch build that should also advance dev-mini's dev-latest
# pointer. No-op when dev-latest is already the FLOATING_TAG (main,
# untagged HEAD) to avoid tagging the image twice.
ifeq ($(FORCE_DEV_LATEST),1)
  ifneq ($(FLOATING_TAG),dev-latest)
    EXTRA_TAG := dev-latest
  else
    EXTRA_TAG :=
  endif
else
  EXTRA_TAG :=
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

# NO_PUSH=1 builds a single-arch image loaded into the local Docker daemon
# instead of a multi-arch image pushed to the registry -- buildx can't --load
# a multi-platform build, so this trades arch coverage for a local image you
# can `docker run` to sanity-check before a real push.
HOST_PLATFORM := linux/$(shell uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
ifneq ($(NO_PUSH),)
  PLATFORM  := $(HOST_PLATFORM)
  PUSH_FLAG := --load
else
  PLATFORM  := linux/amd64,linux/arm64
  PUSH_FLAG := --push
endif

.PHONY: container-build print-tags

all: container-build

container-build:
	@echo "Building $(IMAGE):$(BASE_TAG)$(if $(FLOATING_TAG), [+$(FLOATING_TAG)])$(if $(EXTRA_TAG), [+$(EXTRA_TAG)])$(if $(NO_PUSH), [no-push -- $(HOST_PLATFORM) only])"
	docker buildx build \
		-f Dockerfile \
		--network=host \
		--platform $(PLATFORM) \
		$(TAGS) \
		$(PUSH_FLAG) .
	@echo "Done."

print-tags:
	@echo $(BASE_TAG) $(FLOATING_TAG) $(EXTRA_TAG)
