MODULE   := github.com/swenske/gotochanger
VERSION  ?= $(shell dpkg-parsechangelog -SVersion 2>/dev/null || echo 0.1.0)
GOFLAGS  := -trimpath
LDFLAGS  := -s -w -X main.version=$(VERSION)
BUILDMODE := pie

BIN_DIR  := bin
BINARIES := gotochangerd gotochanger-changer gotochangerctl gotochanger-tcmud

GUIDE_SRC   := docs/guide
GUIDE_EMBED := internal/api/static/guide
SITE_OUT    := site

.PHONY: all build test vet fmt clean install guide site

all: build

# Regenerates the embedded User Guide (internal/api/static/guide/index.html)
# from docs/guide/**.md before every build, so a local build is never stale
# relative to the Markdown source. CI's build/test/lint jobs call `go
# build`/`go test` directly (never through make), so they instead rely on
# that generated file being committed and check it's not stale via the
# "guide" drift-check step in .github/workflows/ci.yml's lint job.
guide:
	go run ./tools/docgen -target=embed -src=$(GUIDE_SRC) -out=$(GUIDE_EMBED)

# Exports the same guide content as a self-contained static site (relative
# asset links, no dependency on a running gotochangerd) for external
# publishing - see .github/workflows/docs.yml.
site: guide
	go run ./tools/docgen -target=site -src=$(GUIDE_SRC) -out=$(SITE_OUT)

build: guide
	mkdir -p $(BIN_DIR)
	for b in $(BINARIES); do \
		go build $(GOFLAGS) -buildmode=$(BUILDMODE) -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$$b ./cmd/$$b ; \
	done

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l .

clean:
	rm -rf $(BIN_DIR) $(SITE_OUT)

# Used by debian/rules; installs binaries, config, and the systemd units
# under DESTDIR following FHS/Debian conventions. Everything here lands in
# one staged tree (debian/rules points DESTDIR at debian/tmp) which
# debian/gotochanger.install and debian/gotochanger-kernel.install then
# split between the two binary packages - see debian/rules.
install: build
	install -d $(DESTDIR)/usr/sbin
	install -d $(DESTDIR)/usr/bin
	install -m 0755 $(BIN_DIR)/gotochangerd $(DESTDIR)/usr/sbin/gotochangerd
	install -m 0755 $(BIN_DIR)/gotochanger-changer $(DESTDIR)/usr/bin/gotochanger-changer
	install -m 0755 $(BIN_DIR)/gotochangerctl $(DESTDIR)/usr/bin/gotochangerctl
	install -m 0755 $(BIN_DIR)/gotochanger-tcmud $(DESTDIR)/usr/sbin/gotochanger-tcmud
	install -d $(DESTDIR)/etc/gotochanger
	install -m 0640 configs/gotochanger.yaml $(DESTDIR)/etc/gotochanger/config.yaml
	install -d $(DESTDIR)/usr/lib/systemd/system
	install -m 0644 systemd/gotochanger.service $(DESTDIR)/usr/lib/systemd/system/gotochanger.service
	install -m 0644 systemd/gotochanger-tcmud@.service $(DESTDIR)/usr/lib/systemd/system/gotochanger-tcmud@.service
	install -d $(DESTDIR)/etc/modules-load.d
	install -m 0644 configs/gotochanger-kernel-modules.conf $(DESTDIR)/etc/modules-load.d/gotochanger-kernel.conf
	install -d $(DESTDIR)/usr/share/polkit-1/rules.d
	install -m 0644 configs/gotochanger-kernel.rules $(DESTDIR)/usr/share/polkit-1/rules.d/gotochanger-kernel.rules
	install -d $(DESTDIR)/var/lib/gotochanger
