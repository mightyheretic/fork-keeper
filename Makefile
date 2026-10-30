# Build and install Fork Keeper as a native systemd service.
#   make && sudo make install     (or use deploy/arch/PKGBUILD on Arch)

PREFIX     ?= /usr
SYSCONFDIR ?= /etc
DESTDIR    ?=
GO         ?= go
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILDFLAGS ?= -trimpath -mod=readonly -buildmode=pie

UNITDIR     = $(PREFIX)/lib/systemd/system
SYSUSERSDIR = $(PREFIX)/lib/sysusers.d
TMPFILESDIR = $(PREFIX)/lib/tmpfiles.d

.PHONY: all build test install uninstall clean

all: build

build:
	CGO_ENABLED=0 $(GO) build $(BUILDFLAGS) -ldflags "-s -w -X main.version=$(VERSION)" -o bin/forkkeeper ./cmd/forkkeeper

test:
	$(GO) test -race ./...

# The config and env file are only installed when missing, so an upgrade
# never overwrites local settings.
install:
	install -Dm755 bin/forkkeeper $(DESTDIR)$(PREFIX)/bin/forkkeeper
	install -Dm755 deploy/systemd/forkkeeperctl $(DESTDIR)$(PREFIX)/bin/forkkeeperctl
	install -Dm644 deploy/systemd/forkkeeper.service $(DESTDIR)$(UNITDIR)/forkkeeper.service
	install -Dm644 deploy/systemd/forkkeeper.sysusers $(DESTDIR)$(SYSUSERSDIR)/forkkeeper.conf
	install -Dm644 deploy/systemd/forkkeeper.tmpfiles $(DESTDIR)$(TMPFILESDIR)/forkkeeper.conf
	install -dm755 $(DESTDIR)$(SYSCONFDIR)/forkkeeper
	[ -e $(DESTDIR)$(SYSCONFDIR)/forkkeeper/config.yaml ] || \
		install -m640 config/forkkeeper.example.yaml $(DESTDIR)$(SYSCONFDIR)/forkkeeper/config.yaml
	[ -e $(DESTDIR)$(SYSCONFDIR)/forkkeeper/forkkeeper.env ] || \
		install -m640 deploy/systemd/forkkeeper.env $(DESTDIR)$(SYSCONFDIR)/forkkeeper/forkkeeper.env
	install -Dm644 config/forkkeeper.example.yaml $(DESTDIR)$(PREFIX)/share/doc/forkkeeper/config.example.yaml
	install -Dm644 README.md $(DESTDIR)$(PREFIX)/share/doc/forkkeeper/README.md
	install -Dm644 LICENSE $(DESTDIR)$(PREFIX)/share/licenses/forkkeeper/LICENSE

uninstall:
	rm -f $(DESTDIR)$(PREFIX)/bin/forkkeeper $(DESTDIR)$(PREFIX)/bin/forkkeeperctl \
		$(DESTDIR)$(UNITDIR)/forkkeeper.service $(DESTDIR)$(SYSUSERSDIR)/forkkeeper.conf \
		$(DESTDIR)$(TMPFILESDIR)/forkkeeper.conf
	rm -rf $(DESTDIR)$(PREFIX)/share/doc/forkkeeper $(DESTDIR)$(PREFIX)/share/licenses/forkkeeper

clean:
	rm -rf bin
