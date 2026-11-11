.PHONY: tunnel apk docker-image test tunnel-test shared-test vkfallback-test itest

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
# Bound from bind/, so tunnel and vkfallback share one Go runtime. No go mod
# verify there: it fails with "missing ziphash" on tunnel, whose module path has
# no dot. android/ also holds the signing key and rc-sa.json: mount only the
# modules.
#
# TUNNEL_TAGS: more build tags, comma-separated. vkfake swaps olcrtc for a stand-in
# (vkfallback/fake.go) for the emulator test: rebuild without it before anything else.
comma := ,
tunnel: docker-image
	mkdir -p app/libs
	RC_KEY=$$(sed -n 's/^rc\.key[[:space:]]*=[[:space:]]*//p' local.properties 2>/dev/null | tr -d ' \r'); \
	if [ -z "$$RC_KEY" ]; then echo "warning: no rc.key in local.properties, the app will not decrypt its endpoints"; \
	elif ! echo "$$RC_KEY" | grep -qE '^[0-9a-fA-F]{64}$$'; then echo "rc.key: expected 64 hex chars"; exit 1; fi; \
	RC_KEY=$$RC_KEY docker run --rm --platform linux/amd64 \
		-v $(CURDIR)/tunnel:/src/tunnel -v $(CURDIR)/shared:/src/shared:ro \
		-v $(CURDIR)/vkfallback:/src/vkfallback:ro -v $(CURDIR)/bind:/src/bind:ro \
		-w /src/bind -v tunnel-gomod:/go/pkg/mod -e RC_KEY \
		$(DOCKER_IMAGE):$(DOCKER_TAG) \
		sh -c 'trap "rm -f ../tunnel/key_gen.go" EXIT; \
			if [ -n "$$RC_KEY" ]; then \
				printf "package tunnel\n\nfunc init() { endpointsKey = []byte{%s} }\n" "$$(echo $$RC_KEY | sed "s/../0x&,/g")" > ../tunnel/key_gen.go; fi; \
			(cd ../tunnel && go mod verify) && (cd ../vkfallback && go mod verify) && go mod tidy -diff && \
			gomobile bind -target=android/arm64 -androidapi 26 -tags olcrtc_lean,olcrtc_vkcalls$(if $(TUNNEL_TAGS),$(comma)$(TUNNEL_TAGS)) \
				-ldflags "-checklinkname=0 -extldflags=-Wl,-z,max-page-size=16384" -o ../tunnel/tunnel.aar tunnel splitvpn.local/vkfallback'
	mv tunnel/tunnel.aar app/libs/tunnel.aar

test: rc-test adblock-test tunnel-test shared-test vkfallback-test
	./gradlew testDebugUnitTest -q

# The strategy vector is shared by the tunnel and rc/tool (see desync.go).
tunnel-test:
	cmp tunnel/testdata/strategies.json ../rc/tool/testdata/strategies.json
	docker run --rm --platform linux/amd64 -v $(CURDIR)/tunnel:/src/tunnel -v $(CURDIR)/shared:/src/shared:ro \
		-w /src/tunnel -v tunnel-gomod:/go/pkg/mod $(GO_IMAGE) \
		sh -c 'go mod verify && go mod tidy -diff && go vet ./... && go test -race ./...'

shared-test:
	docker run --rm --platform linux/amd64 -v $(CURDIR)/shared:/src:ro -w /src $(GO_IMAGE) \
		sh -c 'go mod tidy -diff && go vet ./... && go test -race ./...'

# Links the real olcrtc under the same flags as the AAR, but never starts it.
# android/ also holds the signing key and rc-sa.json: mount only the two modules.
# cpu.all=off: x/crypto's AVX2 assembler fails under Rosetta, and this binary
# links it (see private/plans/vk-fallback.md, stage 0).
VKFALLBACK_FLAGS := -tags=olcrtc_lean,olcrtc_vkcalls -ldflags=-checklinkname=0
VKFAKE_FLAGS := -tags=olcrtc_lean,olcrtc_vkcalls,vkfake -ldflags=-checklinkname=0
vkfallback-test:
	docker run --rm --platform linux/amd64 -e GODEBUG=cpu.all=off \
		-v $(CURDIR)/shared:/src/shared:ro -v $(CURDIR)/vkfallback:/src/vkfallback:ro \
		-w /src/vkfallback -v tunnel-gomod:/go/pkg/mod $(GO_IMAGE) \
		sh -c 'go mod verify && go mod tidy -diff && go vet $(VKFALLBACK_FLAGS) ./... && go test -race $(VKFALLBACK_FLAGS) ./... && \
			go vet $(VKFAKE_FLAGS) ./... && go test -race $(VKFAKE_FLAGS) -run TestFake ./...'

# Instrumented: real TUN, emulator only (consent tap, svc wifi).
# Against the local stand (testenv/): relay + nginx in Docker on the host,
# reached from the emulator at 10.0.2.2. The app stays installed afterwards.
# The stand goes down afterwards either way, the exit code is gradle's.
itest:
	$(MAKE) -C testenv up
	ANDROID_SERIAL=$${ANDROID_SERIAL:-emulator-5556} ./gradlew connectedDebugAndroidTest \
		-Pandroid.injected.androidTest.leaveApksInstalledAfterRun=true \
		-Pandroid.testInstrumentationRunnerArguments.endpoint='stand|test.local|10.0.2.2|8443|/app/test' \
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
# The build in the tree: rc-push right after its publish-local must carry its versionName.
rc-push: RC_ARGS = -e APP_VERSION=$(VERSION_CODE):$(VERSION_NAME)
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

# tunnel/adblock.txt, the ad domains (tool in adblock/). By hand before a
# release, like updateRoutes; only adblock/ goes into the container. A
# temporary file, so a failed download leaves the old list alone.
.PHONY: adblock-list adblock-test
adblock-list:
	docker run --rm --platform linux/amd64 -v $(CURDIR)/adblock:/adblock:ro -w /adblock $(GO_ALPINE) \
		go run . > tunnel/adblock.txt.tmp && mv tunnel/adblock.txt.tmp tunnel/adblock.txt \
		|| { rm -f tunnel/adblock.txt.tmp; exit 1; }

adblock-test:
	docker run --rm --platform linux/amd64 -v $(CURDIR)/adblock:/adblock:ro -w /adblock $(GO_ALPINE) \
		sh -c 'go vet ./... && go test ./...'

# The download page's script, docs/download.js: node:test, no npm packages.
NODE_ALPINE := node:22.23.4-alpine@sha256:0e1754758c2df4cfa3e68d59e2e1b7374bb7fd9ea6328eaaacf15cc934af8202
.PHONY: page-test
page-test:
	docker run --rm -v $(CURDIR)/../docs:/docs:ro -w /docs $(NODE_ALPINE) \
		node --test test/download.test.mjs

# GitHub releases, built and signed only here (gradle, upload key from
# local.properties): release-local, then publish-local.
# Then rc-push with latest_version of the new release.
REPO := bakuridzeaslan025-max/split-vpn
VERSION_NAME := $(shell sed -n 's/^ *versionName = "\(.*\)"/\1/p' app/build.gradle.kts)
VERSION_CODE := $(shell sed -n 's/^ *versionCode = \([0-9]*\).*/\1/p' app/build.gradle.kts)
REL_APK := build/split-vpn-$(VERSION_NAME).apk
REL_ASSETS := $(REL_APK) $(REL_APK).sha256 build/split-vpn.apk
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

.PHONY: release-local publish-local release-notes clean-tree
# A release is built from a commit: no local edits, no untracked files.
clean-tree:
	@test -z "$$(git status --porcelain)" || { git status --short; echo "commit or stash first"; exit 1; }

# override: from the environment or the command line, a tag must not reach a release.
release-local: override TUNNEL_TAGS :=
release-local: clean-tree tunnel
	$(if $(APKSIGNER),,$(error no build-tools/*/apksigner under $(SDK_DIR)))./gradlew assembleRelease
	mkdir -p build
	cp app/build/outputs/apk/release/app-release.apk $(REL_APK)
	cd build && shasum -a 256 $(notdir $(REL_APK)) > $(notdir $(REL_APK)).sha256
	@$(call VERIFY_APK,$(REL_APK))
	@cat $(REL_APK).sha256

# Publishes the release-local artifacts; the tag must already be pushed.
# The download page (docs/) tries the mirrors in turn: GitHub Pages, GitLab
# Pages, then Releases. Each Pages mirror gets PAGES_FILES, Releases the APKs
# and the checksum. A re-run after a failure repeats it and, for a release
# that already exists (gh leaves a draft when an upload fails), uploads the
# assets again and publishes it. Fails unless the versioned file on each
# mirror has the release's SHA-256: rc-push only after that.
publish-local: clean-tree
	$(need_tag)$(if $(filter v$(VERSION_NAME),$(TAG)),,$(error TAG=$(TAG), but versionName is $(VERSION_NAME)))
	@test "$$(git rev-parse '$(TAG)^{commit}')" = "$$(git rev-parse HEAD)" || { echo "$(TAG) is not HEAD"; exit 1; }
	@test -s release-notes/$(TAG).md || { echo "no release-notes/$(TAG).md: write what changed, in Russian"; exit 1; }
	@$(call VERIFY_APK,$(REL_APK))
	cd build && shasum -a 256 -c $(notdir $(REL_APK)).sha256
	cp $(REL_APK) build/split-vpn.apk
	head -c 32768 /dev/urandom > build/probe.bin
	$(call release_notes,$(TAG))
	@$(call push_dl,$(TAG),$(PAGES_FILES))
	@$(call push_gl,$(TAG),$(PAGES_FILES))
	if gh release view $(TAG) --repo $(REPO) >/dev/null 2>&1; then \
		gh release upload $(TAG) --repo $(REPO) --clobber $(REL_ASSETS) && gh release edit $(TAG) --repo $(REPO) --draft=false; \
	else gh release create $(TAG) --repo $(REPO) --verify-tag --title $(TAG) --notes-file build/notes.md $(REL_ASSETS); fi
	@$(call check_mirror,$(DL_URL)/$(notdir $(REL_APK)))
	@$(call check_probe,$(DL_URL))
	@$(call check_mirror,$(GL_URL)/$(notdir $(REL_APK)),$(GL_HINT))
	@$(call check_probe,$(GL_URL),$(GL_HINT))
	@$(call check_mirror,https://github.com/$(REPO)/releases/download/$(TAG)/$(notdir $(REL_APK)))

# What both Pages mirrors serve: the APK under both names and probe.bin, 32 KB
# of random bytes, which no CDN can compress. The download page reads it to
# tell a slow link from a host throttled after the first ~16 KB; a mirror
# without it is never picked. A push replaces the whole tree, so every push
# carries all three.
PAGES_FILES := $(REL_APK) build/split-vpn.apk build/probe.bin

# One parentless commit, message $(1), force-pushed to main of $(2) from a
# throwaway repo, no clone: Pages serves only the current tree, old APKs drop
# out. $(3) fills the work tree "$$w", $(4) is the credential helper. The
# mirror repos are public, so the author is not the global git user. The
# exports come after git init, which fails with GIT_WORK_TREE set.
push_tree = set -e; tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; w="$$tmp/w"; mkdir "$$w"; \
	git init -q --bare "$$tmp/git"; $(3); \
	export GIT_DIR="$$tmp/git" GIT_WORK_TREE="$$w" GIT_TERMINAL_PROMPT=0; git add -A; \
	c=$$(echo $(1) | git -c user.name="Split VPN" -c user.email="noreply@users.noreply.github.com" commit-tree $$(git write-tree)); \
	git -c credential.helper= -c credential.helper='$(4)' push -q --force $(2) $$c:refs/heads/main; \
	echo "$(2): main is $$c"

# The APK mirror on GitHub Pages, a repo of its own so this one does not grow
# by an APK per release. HTTPS through gh, never SSH: the Mac's SSH key is
# another account.
DL_REPO := bakuridzeaslan025-max/split-vpn-dl
DL_URL := https://bakuridzeaslan025-max.github.io/split-vpn-dl
push_dl = $(call push_tree,$(1),https://github.com/$(DL_REPO).git,touch "$$w/.nojekyll"; cp $(2) "$$w/",!gh auth git-credential)

# The APK mirror on GitLab Pages, for when GitHub Pages is down or throttled.
# Pages is the CI job's public/ artifact, live 1-3 minutes after the push.
# local.properties: gitlab.token (a PAT with write_repository); it goes to git
# through the environment and a credential helper, never argv. The PAT
# expires (a year at most; the current one on 2027-11-08). Preconditions in
# the GitLab project: main, protected as the default branch, allows force
# push; the unique Pages domain stays on, GL_URL is hard-coded in
# docs/index.html too.
GL_REPO := bakuridzeaslan025/split-vpn-dl
GL_URL := https://split-vpn-dl-547fcb.gitlab.io
# A failed pipeline looks like a slow one from outside: 404 until check_mirror gives up.
GL_HINT := if the pipeline failed: https://gitlab.com/$(GL_REPO)/-/pipelines
GL_IMAGE := alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
gl_tree = mkdir "$$w/public"; cp $(1) "$$w/public/"; printf '%s\n' 'pages:' '  image: $(GL_IMAGE)' \
	'  script: ["ls -l public"]' '  artifacts:' '    paths: [public]' '  rules:' \
	'    - if: $$CI_COMMIT_BRANCH == $$CI_DEFAULT_BRANCH' > "$$w/.gitlab-ci.yml"
push_gl = GL_TOKEN=$$($(call RC_PROP,gitlab\.token)); \
	[ -n "$$GL_TOKEN" ] || { echo "no gitlab.token in local.properties"; exit 1; }; export GL_TOKEN; \
	$(call push_tree,$(1),https://gitlab.com/$(GL_REPO).git,$(call gl_tree,$(2)),!f() { echo username=oauth2; echo "password=$$GL_TOKEN"; }; f)

.PHONY: pages-deploy
# The release-local files to both Pages mirrors, Releases untouched: to
# back-fill a published release after the mirrors' layout changed.
pages-deploy:
	$(need_tag)$(if $(filter v$(VERSION_NAME),$(TAG)),,$(error TAG=$(TAG), but versionName is $(VERSION_NAME)))
	@$(call VERIFY_APK,$(REL_APK))
	cd build && shasum -a 256 -c $(notdir $(REL_APK)).sha256
	cp $(REL_APK) build/split-vpn.apk
	head -c 32768 /dev/urandom > build/probe.bin
	@$(call push_dl,$(TAG),$(PAGES_FILES))
	@$(call push_gl,$(TAG),$(PAGES_FILES))
	@$(call check_mirror,$(DL_URL)/$(notdir $(REL_APK)))
	@$(call check_probe,$(DL_URL))
	@$(call check_mirror,$(GL_URL)/$(notdir $(REL_APK)),$(GL_HINT))
	@$(call check_probe,$(GL_URL),$(GL_HINT))

# Polls the Pages mirror $(1) like check_mirror until probe.bin answers 200
# with all 32768 bytes. Size only: probe.bin is random per push, and a check
# can still get the one of the tree before. check_mirror may pass on that
# older tree too, when the APK did not change.
check_probe = for i in $$(seq 40); do \
		r=$$(curl -sL --max-time 60 -o /dev/null -w '%{http_code} %{size_download}' '$(1)/probe.bin?t='$$(date +%s)) || r="curl exit code $$?"; \
		[ "$$r" = "200 32768" ] && { echo "$(1)/probe.bin: ok"; exit 0; }; \
		echo "$(1)/probe.bin: $$r, try $$i of 40"; sleep 15; done; \
	echo "$(1)/probe.bin: no 200 with 32768 bytes in 40 tries, the last one: $$r"; $(if $(2),echo "$(2)";) exit 1

# Polls $(1) until it downloads, 40 tries 15 s apart: a Pages build takes
# seconds to minutes, release downloads have answered 504 for hours. A file
# that downloads with another SHA-256 fails at once. Versioned names only:
# Pages serves split-vpn.apk with max-age=600, a check could get the old one.
# The query keeps a cached 404 from the CDN out of the way. $(2), if given,
# is printed when the tries run out.
check_mirror = f=$$(mktemp); trap 'rm -f "$$f"' EXIT; want=$$(cut -d' ' -f1 $(REL_APK).sha256); \
	for i in $$(seq 40); do \
		code=$$(curl -sL --max-time 120 -o "$$f" -w '%{http_code}' '$(1)?t='$$(date +%s)); rc=$$?; \
		if [ $$rc = 0 ] && [ "$$code" = 200 ]; then got=$$(shasum -a 256 < "$$f" | cut -d' ' -f1); \
			[ "$$got" = "$$want" ] && { echo "$(1): SHA-256 ok"; exit 0; }; \
			echo "$(1): SHA-256 $$got, expected $$want"; exit 1; fi; \
		if [ $$rc = 0 ]; then last="HTTP $$code"; else last="curl exit code $$rc"; fi; \
		echo "$(1): $$last, try $$i of 40"; sleep 15; done; \
	echo "$(1): no download in 40 tries, the last one: $$last"; $(if $(2),echo "$(2)";) exit 1

# Rewrites the text of an already published release from release-notes/.
release-notes:
	$(need_tag)@test -s release-notes/$(TAG).md || { echo "no release-notes/$(TAG).md"; exit 1; }
	$(call release_notes,$(TAG))
	gh release edit $(TAG) --repo $(REPO) --notes-file build/notes.md

release_notes = mkdir -p build && { cat release-notes/$(1).md; printf '\nSHA-256 сертификата: %s\n' '$(CERT_SHA256)'; } > build/notes.md
