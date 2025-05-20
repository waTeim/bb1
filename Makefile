# Binary name
BINARY := bb1

# Version tag (short git SHA)
VERSION := v1.0.0

# Full image name (override REGISTRY as needed)
REGISTRY ?= docker.io/wateim
IMAGE := $(REGISTRY)/$(BINARY):$(VERSION)

.PHONY: build docs docker-build docker-push

## docs: generate Swagger documentation in docs/
docs:
	@echo "→ Generating Swagger docs..."
	swag init --generalInfo main.go --output docs

## build: compile a static Linux binary into bin/
build:
	@echo "→ Building $(BINARY)..."
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o bin/$(BINARY) main.go

docker-build:
	@echo "→ Building Docker image $(IMAGE)..."
	docker build --platform=linux/amd64 -t $(IMAGE) .

## push: build Docker image and push to registry
docker-push: 
	@echo "→ Pushing Docker image..."
	docker push $(IMAGE)
