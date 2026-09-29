.PHONY: tunnel apk docker-image test tunnel-test itest

DOCKER_IMAGE := tunnel-builder
DOCKER_TAG := latest
# One Go release everywhere: the AAR's image (pinned in Dockerfile.build) for
# its tests, the alpine build of it for rc/tool and relay/Makefile.
GO_IMAGE := $(shell sed -n 's/^ARG GO_IMAGE=//p' Dockerfile.build)
$(if $(GO_IMAGE),,$(error no ARG GO_IMAGE= in Dockerfile.build))
XMOBILE := $(shell awk '$$1 == "golang.org/x/mobile" { print $$2 }' tunnel/go.mod)
GO_ALPINE := golang:1.27.1-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414
docker-image:
	$(if $(XMOBILE),,$(error no golang.org/x/mobile in tunnel/go.mod))docker build --platform linux/amd64 --build-arg XMOBILE=$(XMOBILE) -f Dockerfile.build -t $(DOCKER_IMAGE):$(DOCKER_TAG) .

# max-page-size=16384: Play requires 16 KB page alignment for native libs.
# rc.key (local.properties) decrypts the endpoint list baked in by gradle. It
# goes in through the environment, so make does not echo it, and into the
# build as key_gen.go with raw bytes, deleted right after: -ldflags -X would
# also leave it as text in the .so's build info. Without it the build still
# works, the app just has no endpoints.
tunnel: docker-image
	mkdir -p app/libs
	RC_KEY=$$(sed -n 's/^rc\.key[[:space:]]*=[[:space:]]*//p' local.properties 2>/dev/null | tr -d ' \r'); \
	if [ -z "$$RC_KEY" ]; then echo "warning: no rc.key in local.properties, the app will not decrypt its endpoints"; \
	elif ! echo "$$RC_KEY" | grep -qE '^[0-9a-fA-F]{64}$$'; then echo "rc.key: expected 64 hex chars"; exit 1; fi; \
	RC_KEY=$$RC_KEY docker run --rm --platform linux/amd64 \
		-v $(CURDIR)/tunnel:/src -v tunnel-gomod:/go/pkg/mod -e RC_KEY \
		$(DOCKER_IMAGE):$(DOCKER_TAG) \
		sh -c 'trap "rm -f key_gen.go" EXIT; \
			if [ -n "$$RC_KEY" ]; then \
				printf "package tunnel\n\nfunc init() { endpointsKey = []byte{%s} }\n" "$$(echo $$RC_KEY | sed "s/../0x&,/g")" > key_gen.go; fi; \
			go mod verify && gomobile bind -target=android/arm64 -androidapi 26 -ldflags "-extldflags=-Wl,-z,max-page-size=16384" -o tunnel.aar .'
	mv tunnel/tunnel.aar app/libs/tunnel.aar

test: rc-test tunnel-test
	./gradlew testDebugUnitTest -q

tunnel-test:
	docker run --rm --platform linux/amd64 -v $(CURDIR)/tunnel:/src -w /src -v tunnel-gomod:/go/pkg/mod $(GO_IMAGE) \
		sh -c 'go mod verify && go mod tidy -diff && go vet ./... && go test -race ./...'

# Instrumented: real TUN, emulator only (consent tap, svc wifi).
# Against the local stand (testenv/): relay + nginx in Docker on the host,
# reached from the emulator at 10.0.2.2. The app stays installed afterwards.
# The stand goes down afterwards either way, the exit code is gradle's.
itest:
	$(MAKE) -C testenv up
	ANDROID_SERIAL=$${ANDROID_SERIAL:-emulator-5554} ./gradlew connectedDebugAndroidTest \
		-Pandroid.injected.androidTest.leaveApksInstalledAfterRun=true \
		-Pandroid.testInstrumentationRunnerArguments.endpoint='test.local|10.0.2.2|8443|/app/test' \
		-Pandroid.testInstrumentationRunnerArguments.ca=$$(base64 < testenv/certs/ca.crt | tr -d '\n') \
		-Pandroid.testInstrumentationRunnerArguments.invites=$$(paste -sd, testenv/data/invites) \
		-Pandroid.testInstrumentationRunnerArguments.control=10.0.2.2:9099; \
	rc=$$?; $(MAKE) -C testenv down; exit $$rc

apk: tunnel
	./gradlew assembleDebug

# Remote Config (tool in rc/tool/). rc-push writes
# only the build_debug condition, rc-pull prints the decrypted endpoints.
# rc-check tries every build_debug endpoint through its relay, rc-promote
# then copies build_debug into the defaults (release) if all of them work;
# a list equal to the defaults' is promoted unchecked, without an invite.
# Their credential is cached in rc/.check-cred for its 7 days; without one,
# INVITE=<code> (relay/Makefile `make invite` on the VDS).
# local.properties: rc.key, vds.endpoints (unless rc/endpoints.json exists),
# rc.sa = service account key with Remote Config Admin, path from android/.
# The container sees only rc/, google-services.json and the SA key; the two
# secrets go through the environment, not argv.
RC_PROP = sed -n 's/^$(1)[[:space:]]*=[[:space:]]*//p' local.properties 2>/dev/null | tr -d '\r'
RC_RUN = SA=$$($(call RC_PROP,rc\.sa)); \
	if [ ! -f "$$SA" ]; then echo "rc.sa in local.properties: no such file"; exit 1; fi; \
	RC_KEY=$$($(call RC_PROP,rc\.key)) VDS_ENDPOINTS=$$($(call RC_PROP,vds\.endpoints)) \
	docker run --rm --platform linux/amd64 -e RC_KEY -e VDS_ENDPOINTS $(RC_ARGS) \
		-v $(CURDIR)/../rc:/rc:ro -v $(CURDIR)/app/google-services.json:/google-services.json:ro \
		-v "$$(cd "$$(dirname "$$SA")" && pwd)/$$(basename "$$SA")":/sa.json:ro \
		-w /rc/tool $(GO_ALPINE) go run .

.PHONY: rc-push rc-pull rc-rollback rc-test rc-check rc-promote
rc-push:
	@$(RC_RUN) push

rc-pull:
	@$(RC_RUN) pull

rc-rollback:
	@if [ -z "$(VERSION)" ]; then echo "usage: make rc-rollback VERSION=n"; exit 1; fi
	@$(RC_RUN) rollback $(VERSION)

# The file exists before the mount, or docker makes a directory of it.
rc-check rc-promote: RC_ARGS = -e INVITE -v $(CURDIR)/../rc/.check-cred:/check-cred
rc-check rc-promote:
	@touch ../rc/.check-cred && chmod 600 ../rc/.check-cred
	@$(RC_RUN) $(subst rc-,,$@)

rc-test:
	docker run --rm --platform linux/amd64 -v $(CURDIR)/../rc:/rc -w /rc/tool $(GO_ALPINE) \
		sh -c 'go vet ./... && go test ./...'

# GitHub releases, built and signed only here (gradle, upload key from
# local.properties): release-local, then publish-local.
# Then rc-push with latest_version and update_url of the new release.
REPO := bakuridzeaslan025-max/split-vpn
VERSION_NAME := $(shell sed -n 's/^ *versionName = "\(.*\)"/\1/p' app/build.gradle.kts)
REL_APK := build/split-vpn-$(VERSION_NAME).apk
# SHA-256 of the upload key's certificate, also in README.
CERT_SHA256 := 4cf035d2f09dab9f1781d6e5af593b7d5ef59dea15b45adfe4a596027fd2f8c4
SDK_DIR := $(or $(ANDROID_HOME),$(shell $(call RC_PROP,sdk\.dir)))
BUILD_TOOLS := $(lastword $(sort $(wildcard $(SDK_DIR)/build-tools/*/apksigner)))
APKSIGNER := $(BUILD_TOOLS)
# Fails unless the APK has exactly one signer and it is our certificate.
VERIFY_APK = set -o pipefail; certs=$$($(APKSIGNER) verify --print-certs $(1)) && echo "$$certs" \
	&& [ "$$(echo "$$certs" | grep -c '^Signer \#[0-9]* certificate SHA-256 digest: ')" = 1 ] \
	&& echo "$$certs" | grep -qx 'Signer \#1 certificate SHA-256 digest: $(CERT_SHA256)' \
	|| { echo "$(1): not signed with the upload key alone"; exit 1; }
need_tag = $(if $(shell echo '$(TAG)' | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$$'),,$(error usage: make $@ TAG=vX.Y.Z))

.PHONY: release-local publish-local clean-tree
# A release is built from a commit: no local edits, no untracked files.
clean-tree:
	@test -z "$$(git status --porcelain)" || { git status --short; echo "commit or stash first"; exit 1; }

release-local: clean-tree tunnel
	$(if $(APKSIGNER),,$(error no build-tools/*/apksigner under $(SDK_DIR)))./gradlew assembleRelease
	mkdir -p build
	cp app/build/outputs/apk/release/app-release.apk $(REL_APK)
	cd build && shasum -a 256 $(notdir $(REL_APK)) > $(notdir $(REL_APK)).sha256
	@$(call VERIFY_APK,$(REL_APK))
	@cat $(REL_APK).sha256

# Publishes the release-local artifacts; the tag must already be pushed.
publish-local: clean-tree
	$(need_tag)$(if $(filter v$(VERSION_NAME),$(TAG)),,$(error TAG=$(TAG), but versionName is $(VERSION_NAME)))
	@test "$$(git rev-parse '$(TAG)^{commit}')" = "$$(git rev-parse HEAD)" || { echo "$(TAG) is not HEAD"; exit 1; }
	@$(call VERIFY_APK,$(REL_APK))
	cd build && shasum -a 256 -c $(notdir $(REL_APK)).sha256
	gh release create $(TAG) --repo $(REPO) --verify-tag --title $(TAG) --notes "SHA-256 сертификата: $(CERT_SHA256)" $(REL_APK) $(REL_APK).sha256
