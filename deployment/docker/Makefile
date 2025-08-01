# All these commands must run from repository root.

DOCKER_REGISTRIES ?= docker.io quay.io
DOCKER_NAMESPACE ?= hub.mobgi.com/infra

ROOT_IMAGE ?= registry.cn-hongkong.aliyuncs.com/person_infra/alpine:3.22.0
ROOT_IMAGE_SCRATCH ?= scratch
CERTS_IMAGE := registry.cn-hongkong.aliyuncs.com/person_infra/alpine:3.22.0

# golang基础构建环境
GO_BUILDER_IMAGE := registry.cn-hongkong.aliyuncs.com/person_infra/golang:1.24.5-alpine
BUILDER_IMAGE := local/builder:2.0.0-$(shell echo $(GO_BUILDER_IMAGE) | tr :/ __)-1
BASE_IMAGE := local/base:1.1.6-$(shell echo $(ROOT_IMAGE) | tr :/ __)
DOCKER ?= docker
DOCKER_RUN ?= $(DOCKER) run
DOCKER_BUILD ?= $(DOCKER) build
DOCKER_COMPOSE ?= $(DOCKER) compose
DOCKER_IMAGE_LS ?= $(DOCKER) image ls --format '{{.Repository}}:{{.Tag}}'


package-base:
	($(DOCKER_IMAGE_LS) | grep -q '$(BASE_IMAGE)$$') \
		|| $(DOCKER_BUILD) \
			--build-arg root_image=$(ROOT_IMAGE) \
			--build-arg certs_image=$(CERTS_IMAGE) \
			--tag $(BASE_IMAGE) \
			deployment/docker/base

package-builder:
	($(DOCKER_IMAGE_LS) | grep -q '$(BUILDER_IMAGE)$$') \
		|| $(DOCKER_BUILD) \
			--build-arg go_builder_image=$(GO_BUILDER_IMAGE) \
			--build-arg gitpass=$(GITPASS) \
			--tag $(BUILDER_IMAGE) \
			deployment/docker/builder

pacakge-builder-refresh:
	$(DOCKER_BUILD) \
			--build-arg go_builder_image=$(GO_BUILDER_IMAGE) \
			--build-arg gitpass="$(GITPASS)" \
			--tag $(BUILDER_IMAGE) \
			deployment/docker/builder

####--user $(shell id -u):$(shell id -g) 
app-via-docker: package-builder
	mkdir -p gocache-for-docker
	$(DOCKER_RUN) --rm \
		--mount type=bind,src="$(shell pwd)",dst=/gaea \
		-w /gaea \
		--mount type=bind,src="$(shell pwd)/gocache-for-docker",dst=/gocache \
		--env GOCACHE=/gocache \
		$(DOCKER_OPTS) \
		$(BUILDER_IMAGE) \
		go build $(RACE) -trimpath -buildvcs=false \
			-ldflags "-extldflags '-static' $(GO_BUILDINFO)" \
			-tags 'netgo osusergo musl' \
			-o bin/$(APP_NAME)$(APP_SUFFIX)-prod $(PKG_PREFIX)/cmd/$(APP_NAME)

app-via-docker-windows: package-builder
	mkdir -p gocache-for-docker
	$(DOCKER_RUN) --rm \
		--user $(shell id -u):$(shell id -g) \
		--mount type=bind,src="$(shell pwd)",dst=/gaea \
		-w /gaea \
		--mount type=bind,src="$(shell pwd)/gocache-for-docker",dst=/gocache \
		--env GOCACHE=/gocache \
		$(DOCKER_OPTS) \
		$(BUILDER_IMAGE) \
		go build $(RACE) -trimpath -buildvcs=false \
			-ldflags "-s -w -extldflags '-static' $(GO_BUILDINFO)" \
			-tags 'netgo osusergo' \
			-o bin/$(APP_NAME)-windows$(APP_SUFFIX)-prod.exe $(PKG_PREFIX)/cmd/$(APP_NAME)

package-via-docker: package-base
	($(DOCKER_IMAGE_LS) | grep -q '$(DOCKER_NAMESPACE)/$(APP_NAME):$(PKG_TAG)$(APP_SUFFIX)$(RACE)$$') || (\
		$(MAKE) app-via-docker && \
		$(DOCKER_BUILD) \
			--build-arg src_binary=$(APP_NAME)$(APP_SUFFIX)-prod \
			--build-arg base_image=$(BASE_IMAGE) \
			--label "org.opencontainers.image.source=http://git.ids111.com/idreamsky/advertisement/PlatformProducts/cl_workflow_workers" \
			--label "org.opencontainers.image.title=$(APP_NAME)" \
			--label "org.opencontainers.image.vendor=cl_ad" \
			--label "org.opencontainers.image.version=$(PKG_TAG)" \
			--label "org.opencontainers.image.created=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")" \
			--tag $(DOCKER_NAMESPACE)/$(APP_NAME):$(PKG_TAG)$(APP_SUFFIX)$(RACE) \
			-f cmd/$(APP_NAME)/deployment/Dockerfile bin) && docker push $(DOCKER_NAMESPACE)/$(APP_NAME):$(PKG_TAG)$(APP_SUFFIX)$(RACE)

deploy-via-k8s: package-via-docker
	kubectl apply -k cmd/$(APP_NAME)/deployment/k8s && kubectl -n wf set image deployment/$(APP_NAME) app=$(DOCKER_NAMESPACE)/$(APP_NAME):$(PKG_TAG)$(APP_SUFFIX)$(RACE)

publish-via-docker:
	$(MAKE_PARALLEL) app-via-docker-linux-amd64 \
		app-via-docker-linux-arm \
		app-via-docker-linux-arm64 \
		app-via-docker-linux-ppc64le \
		app-via-docker-linux-386
	$(DOCKER) buildx build \
		--platform=linux/amd64,linux/arm,linux/arm64,linux/ppc64le,linux/386 \
		--build-arg certs_image=$(CERTS_IMAGE) \
		--build-arg root_image=$(ROOT_IMAGE) \
		--build-arg APP_NAME=$(APP_NAME) \
		--build-arg BINARY_SUFFIX="" \
		--label "org.opencontainers.image.source=https://github.com/VictoriaMetrics/VictoriaMetrics" \
		--label "org.opencontainers.image.documentation=https://docs.victoriametrics.com/" \
		--label "org.opencontainers.image.title=$(APP_NAME)" \
		--label "org.opencontainers.image.vendor=VictoriaMetrics" \
		--label "org.opencontainers.image.version=$(PKG_TAG)" \
		--label "org.opencontainers.image.created=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")" \
		$(foreach registry,$(DOCKER_REGISTRIES),\
		--tag $(registry)/$(DOCKER_NAMESPACE)/$(APP_NAME):$(PKG_TAG)$(RACE)$(EXTRA_DOCKER_TAG_SUFFIX) \
		) \
		-o type=image \
		--provenance=false \
		-f cmd/$(APP_NAME)/multiarch/Dockerfile \
		--push \
		bin
	$(DOCKER) buildx build \
		--platform=linux/amd64,linux/arm,linux/arm64,linux/ppc64le,linux/386 \
		--build-arg certs_image=$(CERTS_IMAGE) \
		--build-arg root_image=$(ROOT_IMAGE_SCRATCH) \
		--build-arg APP_NAME=$(APP_NAME) \
		--build-arg BINARY_SUFFIX="" \
		--label "org.opencontainers.image.source=https://github.com/VictoriaMetrics/VictoriaMetrics" \
		--label "org.opencontainers.image.documentation=https://docs.victoriametrics.com/" \
		--label "org.opencontainers.image.title=$(APP_NAME)" \
		--label "org.opencontainers.image.vendor=VictoriaMetrics" \
		--label "org.opencontainers.image.version=$(PKG_TAG)" \
		--label "org.opencontainers.image.created=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")" \
		$(foreach registry,$(DOCKER_REGISTRIES),\
		--tag $(registry)/$(DOCKER_NAMESPACE)/$(APP_NAME):$(PKG_TAG)$(RACE)$(EXTRA_DOCKER_TAG_SUFFIX)-scratch \
		) \
		-o type=image \
		--provenance=false \
		-f cmd/$(APP_NAME)/multiarch/Dockerfile \
		--push \
		bin
	cd bin && rm -rf \
		$(APP_NAME)-linux-amd64-prod \
		$(APP_NAME)-linux-arm-prod \
		$(APP_NAME)-linux-arm64-prod \
		$(APP_NAME)-linux-ppc64le-prod \
		$(APP_NAME)-linux-386-prod

publish-via-docker-from-rc:
	$(foreach registry,$(DOCKER_REGISTRIES),\
		docker buildx imagetools create --tag $(registry)/$(DOCKER_NAMESPACE)/$(APP_NAME):$(PKG_TAG) $(registry)/$(DOCKER_NAMESPACE)/$(APP_NAME):$(PKG_TAG)$(EXTRA_DOCKER_TAG_SUFFIX); \
	)
	$(foreach registry,$(DOCKER_REGISTRIES),\
		docker buildx imagetools create --tag $(registry)/$(DOCKER_NAMESPACE)/$(APP_NAME):$(PKG_TAG)-scratch $(registry)/$(DOCKER_NAMESPACE)/$(APP_NAME):$(PKG_TAG)$(EXTRA_DOCKER_TAG_SUFFIX)-scratch; \
	)

publish-via-docker-latest:
	$(foreach registry,$(DOCKER_REGISTRIES),\
		docker buildx imagetools create --tag $(registry)/$(DOCKER_NAMESPACE)/$(APP_NAME):latest $(registry)/$(DOCKER_NAMESPACE)/$(APP_NAME):$(PKG_TAG); \
	)

run-via-docker: package-via-docker
	$(DOCKER_RUN) -it --rm \
		--user $(shell id -u):$(shell id -g) \
		--net host \
		$(DOCKER_OPTS) \
		$(DOCKER_NAMESPACE)/$(APP_NAME):$(PKG_TAG)$(APP_SUFFIX)$(RACE) $(ARGS)

app-via-docker-goos-goarch:
	APP_SUFFIX='-$(GOOS)-$(GOARCH)' \
	DOCKER_OPTS='--env CGO_ENABLED=$(CGO_ENABLED) --env GOOS=$(GOOS) --env GOARCH=$(GOARCH) $(foreach v,$(EXTRA_DOCKER_ENVS),--env $(v))' \
	$(MAKE) app-via-docker

app-via-docker-pure:
	APP_SUFFIX='-pure' DOCKER_OPTS='--env CGO_ENABLED=0' $(MAKE) app-via-docker

app-via-docker-linux-amd64:
	EXTRA_DOCKER_ENVS='CC=/opt/cross-builder/x86_64-linux-musl-cross/bin/x86_64-linux-musl-gcc' \
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 $(MAKE) app-via-docker-goos-goarch

app-via-docker-linux-arm64:
	EXTRA_DOCKER_ENVS='CC=/opt/cross-builder/aarch64-linux-musl-cross/bin/aarch64-linux-musl-gcc' \
	CGO_ENABLED=1 GOOS=linux GOARCH=arm64 $(MAKE) app-via-docker-goos-goarch

app-via-docker-linux-arm:
	EXTRA_DOCKER_ENVS='GOARM=5' \
	CGO_ENABLED=0 GOOS=linux GOARCH=arm $(MAKE) app-via-docker-goos-goarch

app-via-docker-linux-ppc64le:
	CGO_ENABLED=0 GOOS=linux GOARCH=ppc64le $(MAKE) app-via-docker-goos-goarch

app-via-docker-linux-386:
	CGO_ENABLED=0 GOOS=linux GOARCH=386 $(MAKE) app-via-docker-goos-goarch

app-via-docker-darwin-amd64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(MAKE) app-via-docker-goos-goarch

app-via-docker-darwin-arm64:
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(MAKE) app-via-docker-goos-goarch

app-via-docker-freebsd-amd64:
	CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 $(MAKE) app-via-docker-goos-goarch

app-via-docker-openbsd-amd64:
	CGO_ENABLED=0 GOOS=openbsd GOARCH=amd64 $(MAKE) app-via-docker-goos-goarch

app-via-docker-windows-amd64:
	APP_SUFFIX='-amd64' \
	DOCKER_OPTS='--env CGO_ENABLED=0 --env GOOS=windows --env GOARCH=amd64' \
	$(MAKE) app-via-docker-windows

package-via-docker-goarch:
	APP_SUFFIX='-$(GOARCH)' \
	DOCKER_OPTS='--env CGO_ENABLED=$(CGO_ENABLED) --env GOOS=linux --env GOARCH=$(GOARCH) $(foreach v,$(EXTRA_DOCKER_ENVS),--env $(v))' \
	$(MAKE) package-via-docker

package-via-docker-pure:
	APP_SUFFIX='-pure' DOCKER_OPTS='--env CGO_ENABLED=0' $(MAKE) package-via-docker

package-via-docker-amd64:
	EXTRA_DOCKER_ENVS='CC=/opt/cross-builder/x86_64-linux-musl-cross/bin/x86_64-linux-musl-gcc' \
	CGO_ENABLED=1 GOARCH=amd64 $(MAKE) package-via-docker-goarch

package-via-docker-arm64:
	EXTRA_DOCKER_ENVS='CC=/opt/cross-builder/aarch64-linux-musl-cross/bin/aarch64-linux-musl-gcc' \
	CGO_ENABLED=1 GOARCH=arm64 $(MAKE) package-via-docker-goarch

package-via-docker-arm:
	EXTRA_DOCKER_ENVS='GOARM=5' \
	CGO_ENABLED=0 GOARCH=arm $(MAKE) package-via-docker-goarch

package-via-docker-ppc64le:
	CGO_ENABLED=0 GOARCH=ppc64le $(MAKE) package-via-docker-goarch

package-via-docker-386:
	CGO_ENABLED=0 GOARCH=386 $(MAKE) package-via-docker-goarch

remove-docker-images:
	docker image ls --format '{{.ID}}' | xargs docker image rm -f

# VM single
docker-vm-single-up:
	$(DOCKER_COMPOSE) -f deployment/docker/compose-vm-single.yml up -d

docker-vm-single-down:
	$(DOCKER_COMPOSE) -f deployment/docker/compose-vm-single.yml down -v

# VM cluster
docker-vm-cluster-up:
	$(DOCKER_COMPOSE) -f deployment/docker/compose-vm-cluster.yml up -d

docker-vm-cluster-down:
	$(DOCKER_COMPOSE) -f deployment/docker/compose-vm-cluster.yml down -v

# Command aliases to keep backward-compatibility, as they could have been mentioned on the Internet before the rename.
docker-single-up: docker-vm-single-up
docker-single-down: docker-vm-single-down

docker-cluster-up: docker-vm-cluster-up
docker-cluster-down: docker-vm-cluster-down
