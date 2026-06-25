IMAGE     ?= ghcr.io/arnobkumarsaha/vcd-lb-gc
TAG       ?= latest
PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: build push

build:
	docker buildx build \
		--platform $(PLATFORMS) \
		-t $(IMAGE):$(TAG) \
		.

push:
	docker buildx build \
		--platform $(PLATFORMS) \
		-t $(IMAGE):$(TAG) \
		--push \
		.
