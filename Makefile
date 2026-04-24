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

TAGS := -t $(IMAGE):$(BASE_TAG)
ifneq ($(FLOATING_TAG),)
  TAGS := $(TAGS) -t $(IMAGE):$(FLOATING_TAG)
endif

.PHONY: container-build

all: container-build

container-build:
	@echo "Building $(IMAGE):$(BASE_TAG)$(if $(FLOATING_TAG), [+$(FLOATING_TAG)])"
	docker buildx build \
		-f Dockerfile \
		--network=host \
		--platform linux/amd64,linux/arm64 \
		$(TAGS) \
		--push .
	@echo "Done."
