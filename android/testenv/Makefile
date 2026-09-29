# Local relay for `make itest` (run from android/): certs and issuer key
# once, then compose up with a fresh batch of one-shot invite codes.
OPENSSL = docker run --rm -v $(CURDIR)/certs:/certs alpine/openssl
CONTROL_PORT = 9099

.PHONY: up down invites

up: certs/server.crt .env invites
	docker compose up -d --build
	docker compose restart nginx
	@pkill -f "control.py $(CONTROL_PORT)" 2>/dev/null || true
	@nohup python3 control.py $(CONTROL_PORT) >/dev/null 2>&1 &
	@sleep 2; docker compose ps

down:
	@pkill -f "control.py $(CONTROL_PORT)" 2>/dev/null || true
	docker compose down

# The harness takes one per start(), the relay burns one per registration.
invites:
	@mkdir -p data
	@for i in $$(seq 50); do head -c 12 /dev/urandom | xxd -p; done > data/invites
	@chmod 644 data/invites

.env:
	docker run --rm -v $(CURDIR)/../../relay:/src -w /src golang:1.23-alpine go run . keygen > .env.tmp && mv .env.tmp .env

certs/server.crt:
	mkdir -p certs
	$(OPENSSL) req -x509 -newkey rsa:2048 -nodes -days 3650 -subj "/CN=Split VPN test CA" \
		-keyout /certs/ca.key -out /certs/ca.crt
	$(OPENSSL) req -newkey rsa:2048 -nodes -subj "/CN=test.local" -addext "subjectAltName=DNS:test.local" \
		-keyout /certs/server.key -out /certs/server.csr
	$(OPENSSL) x509 -req -in /certs/server.csr -CA /certs/ca.crt -CAkey /certs/ca.key -CAcreateserial \
		-days 3650 -copy_extensions copy -out /certs/server.crt
	chmod 644 certs/*
