PKG_PREFIX := github.com/XiaoMi/Gaea

MAKE_CONCURRENCY ?= $(shell getconf _NPROCESSORS_ONLN)
MAKE_PARALLEL := $(MAKE) -j $(MAKE_CONCURRENCY)
DATEINFO_TAG ?= $(shell date -u +'%Y%m%d-%H%M%S')
BUILDINFO_TAG ?= $(shell echo $$(git describe --long --all | tr '/' '-')$$( \
	      git diff-index --quiet HEAD -- || echo '-dirty-'$$(git diff-index -u HEAD | openssl sha1 | cut -d' ' -f2 | cut -c 1-8)))

PKG_TAG ?= $(shell git tag -l --points-at HEAD)
ifeq ($(PKG_TAG),)
PKG_TAG := $(BUILDINFO_TAG)
endif

EXTRA_DOCKER_TAG_SUFFIX ?= EXTRA_DOCKER_TAG_SUFFIX

#GO_BUILDINFO = -X 'pkg.mobgi.com/cl_workflow_core/sdk/buildinfo.Version=$(APP_NAME)-$(DATEINFO_TAG)-$(BUILDINFO_TAG)'
GO_BUILDINFO = ""
TAR_OWNERSHIP ?= --owner=1000 --group=1000

.PHONY: $(MAKECMDGOALS)

#include .env
include cmd/*/Makefile
include deployment/*/Makefile


all: \
	gaea-prod \
	gaea-cc-prod 


clean:
	rm -rf bin/*

publish: \
	publish-gaea \
	publish-gaea-cc 

package: \
	package-gaea \
	package-gaea-cc 


publish-final-images:
	PKG_TAG=$(TAG) APP_NAME=toutiao $(MAKE) publish-via-docker-from-rc && \
	PKG_TAG=$(TAG) APP_NAME=gw $(MAKE) publish-via-docker-from-rc && \
	PKG_TAG=$(TAG) $(MAKE) publish-latest

publish-latest:
	PKG_TAG=$(TAG) APP_NAME=toutiao $(MAKE) publish-via-docker-latest && \
	PKG_TAG=$(TAG) APP_NAME=gw $(MAKE) publish-via-docker-latest 



fmt:
	gofmt -l -w -s ./pkg
	gofmt -l -w -s ./app


vet:
	GOEXPERIMENT=synctest go vet ./pkg/...
	go vet ./app/...


check-all: fmt vet golangci-lint govulncheck

clean-checkers: remove-golangci-lint remove-govulncheck

test:
	GOEXPERIMENT=synctest go test ./lib/... ./app/...

test-race:
	GOEXPERIMENT=synctest go test -race ./lib/... ./app/...

test-pure:
	GOEXPERIMENT=synctest CGO_ENABLED=0 go test ./lib/... ./app/...

test-full:
	GOEXPERIMENT=synctest go test -coverprofile=coverage.txt -covermode=atomic ./lib/... ./app/...

test-full-386:
	GOEXPERIMENT=synctest GOARCH=386 go test -coverprofile=coverage.txt -covermode=atomic ./lib/... ./app/...

integration-test: victoria-metrics vmagent vmalert vmauth vmctl vmbackup vmrestore
	go test ./apptest/... -skip="^TestCluster.*"

benchmark:
	GOEXPERIMENT=synctest go test -bench=. ./lib/...
	go test -bench=. ./app/...

benchmark-pure:
	GOEXPERIMENT=synctest CGO_ENABLED=0 go test -bench=. ./lib/...
	CGO_ENABLED=0 go test -bench=. ./app/...

vendor-update:
	go mod tidy -compat=1.24
	go mod vendor

app-local:
	CGO_ENABLED=0 go build $(RACE) -ldflags "$(GO_BUILDINFO)" -o bin/$(APP_NAME)$(RACE) $(PKG_PREFIX)/cmd/$(APP_NAME)

app-local-pure:
	CGO_ENABLED=0 go build $(RACE) -ldflags "$(GO_BUILDINFO)" -o bin/$(APP_NAME)-pure$(RACE) $(PKG_PREFIX)/cmd/$(APP_NAME)

app-local-goos-goarch:
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(RACE) -ldflags "$(GO_BUILDINFO)" -o bin/$(APP_NAME)-$(GOOS)-$(GOARCH)$(RACE) $(PKG_PREFIX)/cmd/$(APP_NAME)

app-local-windows-goarch:
	CGO_ENABLED=0 GOOS=windows GOARCH=$(GOARCH) go build $(RACE) -ldflags "$(GO_BUILDINFO)" -o bin/$(APP_NAME)-windows-$(GOARCH)$(RACE).exe $(PKG_PREFIX)/cmd/$(APP_NAME)

quicktemplate-gen: install-qtc
	qtc

install-qtc:
	which qtc || go install github.com/valyala/quicktemplate/qtc@latest


golangci-lint: install-golangci-lint
	GOEXPERIMENT=synctest golangci-lint run

install-golangci-lint:
	which golangci-lint || curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(shell go env GOPATH)/bin v1.64.7

remove-golangci-lint:
	rm -rf `which golangci-lint`

govulncheck: install-govulncheck
	govulncheck ./...

install-govulncheck:
	which govulncheck || go install golang.org/x/vuln/cmd/govulncheck@latest

remove-govulncheck:
	rm -rf `which govulncheck`

install-wwhrd:
	which wwhrd || go install github.com/frapposelli/wwhrd@latest

check-licenses: install-wwhrd
	wwhrd check -f .wwhrd.yml
