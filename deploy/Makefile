# Own server, see README.md. SSH_HOST and REMOTE_DIR come from the gitignored
# .env.mk (.env.mk.example), the environment wins over it.
-include .env.mk
need = $(if $($(1)),,$(error $(1) is not set: export it or put it into deploy/.env.mk, see .env.mk.example))
ssh = $(call need,SSH_HOST)$(call need,REMOTE_DIR)ssh $(SSH_HOST)

.PHONY: all setup keygen build check-env push up cert relay restart invite

all: build push up

setup:
	$(ssh) 'sh -s' < setup.sh

keygen:
	@$(MAKE) -s -C ../relay keygen

build:
	$(MAKE) -C ../relay build

check-env:
	@test -f .env || { echo ".env missing: cp .env.example .env"; exit 1; }
	@grep -Eq '^DOMAIN=[A-Za-z0-9.-]+$$' .env || { echo ".env: DOMAIN is not set"; exit 1; }
	@grep -Eq '^VPN_PATH=/[A-Za-z0-9_-]{8,}$$' .env || { echo ".env: VPN_PATH must be / and 8+ of [A-Za-z0-9_-]"; exit 1; }
	@grep -Eq '^ISSUER_KEY=[0-9a-f]{64}$$' .env || { echo ".env: ISSUER_KEY is not set (make keygen)"; exit 1; }
	@grep -Eq '^CRED_PUB=([0-9a-f]{64})?$$' .env || { echo ".env: CRED_PUB must be 64 hex or empty"; exit 1; }

# COPYFILE_DISABLE: no macOS ._ files in the archive. tar run by root keeps
# the archived modes whatever the umask, so .env goes in as 600 already.
push: check-env
	$(call need,SSH_HOST)$(call need,REMOTE_DIR)chmod 600 .env && COPYFILE_DISABLE=1 tar -czf - docker-compose.yml .env nginx html \
		-C .. relay/Dockerfile relay/relay \
		| ssh $(SSH_HOST) 'umask 077 && mkdir -p $(REMOTE_DIR)/relay/data && tar -xzf - --no-same-owner -C $(REMOTE_DIR) && chmod 600 $(REMOTE_DIR)/.env'

up:
	$(ssh) 'cd $(REMOTE_DIR) && docker compose up -d --build --remove-orphans'

# The first certificate; certbot renews it by itself. Needs DOMAIN's A record
# pointing here and nginx up. --agree-tos: Let's Encrypt's subscriber agreement.
cert:
	$(ssh) 'cd $(REMOTE_DIR) && docker compose run --rm --entrypoint certbot certbot certonly \
		--webroot -w /var/www/certbot -n --agree-tos --register-unsafely-without-email \
		-d "$$(sed -n "s/^DOMAIN=//p" .env)" && docker compose restart nginx'

# New relay binary: build, upload, recreate only the relay.
relay: build push
	$(ssh) 'cd $(REMOTE_DIR) && docker compose up -d --build relay'

# The relay reads relay/data/sa.json only on start.
restart:
	$(ssh) 'cd $(REMOTE_DIR) && docker compose up -d --force-recreate relay'

# One-shot invite code for a device without Google Play services.
invite:
	@code=$$(openssl rand -hex 12); $(ssh) "echo $$code >> $(REMOTE_DIR)/relay/data/invites" && echo "$$code"
