# Minimal MVP server: one `olcrtc srv` per user, room and key by hand in
# users/<name>.env (users.example.env). The full vkserver is stage 5 of
# private/plans/vk-fallback.md.
#
# ARCH=amd64 for a real VDS. Locally only the native arm64: under Rosetta, amd64's
# AVX2 XChaCha20-Poly1305 in x/crypto computes wrong results and records fail.
#
# Before a VDS: olcrtc's exit does not refuse port 25 as relay does, so close it
# with a firewall rule or the server's IP lands on blacklists.
ARCH ?= arm64
OLCRTC ?= $(HOME)/projects/my/olcrtc
# The one android/vkfallback pins: both ends must speak one protocol.
COMMIT := $(shell sed -n 's|.*olcrtc => .*-\([0-9a-f]\{12\}\)$$|\1|p' ../android/vkfallback/go.mod)
$(if $(COMMIT),,$(error no olcrtc replace in android/vkfallback/go.mod))
GO_IMAGE := golang:1.27.1-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414
IMAGE := vkserver-olcrtc:$(ARCH)
COMPOSE = ARCH=$(ARCH) NAME=$(NAME) docker compose -p vkserver-$(NAME)
ROOM_RE := ^https://vk\.(ru|com)/call/join/[A-Za-z0-9_-]+$$
KEY_RE := ^[0-9a-fA-F]{64}$$

.PHONY: build image config up down logs check native

# Building for amd64 is fine; running it here is what Rosetta breaks.
native:
	$(if $(and $(filter amd64,$(ARCH)),$(filter arm64 aarch64,$(shell uname -m))),$(error ARCH=amd64 does not run on this arm64 host: records would fail under Rosetta))

# Static, and from the commit itself whatever the fork's checkout holds.
build:
	mkdir -p build/src-$(COMMIT)
	git -C $(OLCRTC) archive $(COMMIT) | tar -x -C build/src-$(COMMIT)
	docker run --rm --platform linux/arm64 -v $(CURDIR)/build:/build -w /build/src-$(COMMIT) \
		-v tunnel-gomod:/go/pkg/mod -e CGO_ENABLED=0 -e GOOS=linux -e GOARCH=$(ARCH) $(GO_IMAGE) \
		go build -trimpath -buildvcs=false -tags olcrtc_lean,olcrtc_vkcalls -ldflags "-s -w" \
		-o /build/olcrtc-$(ARCH) ./cmd/olcrtc
	docker run --rm --platform linux/arm64 $(GO_IMAGE) cat /etc/ssl/certs/ca-certificates.crt > build/ca-certificates.crt

image: build
	docker build --platform linux/$(ARCH) --build-arg ARCH=$(ARCH) --label olcrtc.commit=$(COMMIT) -t $(IMAGE) -f Dockerfile build

# Checks before writing, and prints neither value.
config:
	$(if $(NAME),,$(error usage: make config NAME=<name>))
	@set -a; . users/$(NAME).env; set +a; \
	echo "$$ROOM" | grep -qE '$(ROOM_RE)' || { echo "users/$(NAME).env: ROOM is not a VK join link"; exit 1; }; \
	echo "$$KEY" | grep -qE '$(KEY_RE)' || { echo "users/$(NAME).env: KEY is not 64 hex characters"; exit 1; }; \
	umask 077; mkdir -p run; sed -e "s|@ROOM@|$$ROOM|" -e "s|@KEY@|$$KEY|" config.yaml.tmpl > run/$(NAME).yaml

up: native config
	$(COMPOSE) up -d

down:
	$(if $(NAME),,$(error usage: make down NAME=<name>))
	$(COMPOSE) down

logs:
	$(if $(NAME),,$(error usage: make logs NAME=<name>))
	$(COMPOSE) logs -f

# Offline: an invented room and key, no network in the container, so it cannot
# reach VK. The good config must get past validation and die on its first DNS
# lookup; the bad one must stop at validation.
check: native image
	mkdir -p build/check
	docker run --rm --network none --entrypoint /olcrtc $(IMAGE) -version
	sed -e 's|@ROOM@|https://vk.ru/call/join/OFFLINECHECK|' -e "s|@KEY@|$$(printf '%064d' 0)|" config.yaml.tmpl > build/check/good.yaml
	sed -e 's|@ROOM@|https://vk.ru/call/join/OFFLINECHECK|' -e 's|@KEY@|nothex|' config.yaml.tmpl > build/check/bad.yaml
	docker run --rm --network none -v $(CURDIR)/build/check/bad.yaml:/config.yaml:ro $(IMAGE) 2>&1 | grep 'validate config'
	docker run --rm --network none -v $(CURDIR)/build/check/good.yaml:/config.yaml:ro $(IMAGE) 2>&1 | grep -v 'validate config' | grep 'lookup'
