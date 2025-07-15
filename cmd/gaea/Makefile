# All these commands must run from repository root.

gaea:
	APP_NAME=gaea $(MAKE) app-local

gaea-race:
	APP_NAME=gaea RACE=-race $(MAKE) app-local

gaea-prod:
	APP_NAME=gaea $(MAKE) app-via-docker

gaea-pure-prod:
	APP_NAME=gaea $(MAKE) app-via-docker-pure

gaea-linux-amd64-prod:
	APP_NAME=gaea $(MAKE) app-via-docker-linux-amd64

gaea-darwin-amd64-prod:
	APP_NAME=gaea $(MAKE) app-via-docker-darwin-amd64

gaea-darwin-arm64-prod:
	APP_NAME=gaea $(MAKE) app-via-docker-darwin-arm64

gaea-windows-amd64-prod:
	APP_NAME=gaea $(MAKE) app-via-docker-windows-amd64

package-gaea:
	APP_NAME=gaea $(MAKE) package-via-docker

package-gaea-pure:
	APP_NAME=gaea $(MAKE) package-via-docker-pure

package-gaea-amd64:
	APP_NAME=gaea $(MAKE) package-via-docker-amd64



publish-gaea:
	APP_NAME=gaea $(MAKE) publish-via-docker

run-gaea:
	APP_NAME=gaea \
	DOCKER_OPTS='-v $(shell pwd)/app/gaea/:/app/gaea' \
	ARGS='-auth.config=app/gaea/example_config.yml' \
	$(MAKE) run-via-docker

gaea-linux-amd64:
	APP_NAME=gaea CGO_ENABLED=1 GOOS=linux GOARCH=amd64 $(MAKE) app-local-goos-goarch

gaea-linux-arm:
	APP_NAME=gaea CGO_ENABLED=0 GOOS=linux GOARCH=arm $(MAKE) app-local-goos-goarch

gaea-linux-arm64:
	APP_NAME=gaea CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(MAKE) app-local-goos-goarch

gaea-linux-ppc64le:
	APP_NAME=gaea CGO_ENABLED=0 GOOS=linux GOARCH=ppc64le $(MAKE) app-local-goos-goarch

gaea-linux-s390x:
	APP_NAME=gaea CGO_ENABLED=0 GOOS=linux GOARCH=s390x $(MAKE) app-local-goos-goarch

gaea-linux-loong64:
	APP_NAME=gaea CGO_ENABLED=0 GOOS=linux GOARCH=loong64 $(MAKE) app-local-goos-goarch

gaea-linux-386:
	APP_NAME=gaea CGO_ENABLED=0 GOOS=linux GOARCH=386 $(MAKE) app-local-goos-goarch

gaea-darwin-amd64:
	APP_NAME=gaea CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(MAKE) app-local-goos-goarch

gaea-darwin-arm64:
	APP_NAME=gaea CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(MAKE) app-local-goos-goarch

gaea-freebsd-amd64:
	APP_NAME=gaea CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 $(MAKE) app-local-goos-goarch

gaea-openbsd-amd64:
	APP_NAME=gaea CGO_ENABLED=0 GOOS=openbsd GOARCH=amd64 $(MAKE) app-local-goos-goarch

gaea-windows-amd64:
	GOARCH=amd64 APP_NAME=gaea $(MAKE) app-local-windows-goarch

gaea-pure:
	APP_NAME=gaea $(MAKE) app-local-pure
