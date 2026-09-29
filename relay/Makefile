# SSH_HOSTS (user@host of every VDS, space-separated; a lone SSH_HOST still
# works) and REMOTE_DIR (the compose project dir, same on all of them) come
# from the gitignored .env.mk (.env.mk.example, ?=), the environment wins.
-include .env.mk
SSH_HOSTS ?= $(SSH_HOST)
need = $(if $($(1)),,$(error $(1) is not set: export it or put it into relay/.env.mk, see .env.mk.example))

.PHONY: all build deploy restart test keygen invite

GO_ALPINE := golang:1.27.1-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414

all: build deploy restart

build:
	docker run --rm --platform linux/amd64 -v $(CURDIR):/src -w /src $(GO_ALPINE) \
		go build -trimpath -ldflags="-s -w" -o relay .

test:
	docker run --rm --platform linux/amd64 -v $(CURDIR):/src -w /src $(GO_ALPINE) go test ./...

deploy:
	$(call need,SSH_HOSTS)$(call need,REMOTE_DIR)for h in $(SSH_HOSTS); do scp relay $$h:$(REMOTE_DIR)/relay/relay || exit 1; done

restart:
	$(call need,SSH_HOSTS)$(call need,REMOTE_DIR)for h in $(SSH_HOSTS); do ssh $$h "cd $(REMOTE_DIR) && docker compose up -d --build relay" || exit 1; done

# Issuer key pair for credentials: put both lines into vds/.env (ISSUER_KEY
# only on the issuer relay, CRED_PUB on every relay).
keygen:
	docker run --rm --platform linux/amd64 -v $(CURDIR):/src -w /src $(GO_ALPINE) go run . keygen

# One-shot invite code for a device without Google Play services, written to
# every relay (each burns it on its own). On a failure
# the code goes to stderr only, with the hosts that already have it.
invite:
	$(call need,SSH_HOSTS)$(call need,REMOTE_DIR)@code=$$(openssl rand -hex 12); ok=; \
	for h in $(SSH_HOSTS); do \
		ssh $$h "echo $$code >> $(REMOTE_DIR)/relay/data/invites" || { echo "invite: failed on $$h; $$code is already on:$${ok:- none}" >&2; exit 1; }; \
		ok="$$ok $$h"; \
	done; echo "$$code"
