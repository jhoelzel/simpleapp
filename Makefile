# VERSION defines the project version for the bundle.
# Update this value when you upgrade the version of your project.
VERSION ?= 0.0.5
NAME = $(shell basename "`pwd`")
GITBASEURL = github.com/jhoelzel
CONTAINER_REPOSITORY=ghcr.io
## Accountname ends in / because there are none on some registrys
CONTAINER_REPOSITORY_ACCOUNTNAME=jhoelzel/
CONTAINER_IMAGE_NAME=webrequesttest
IMAGE_REPOSITORY=$(CONTAINER_REPOSITORY)/$(CONTAINER_REPOSITORY_ACCOUNTNAME)$(CONTAINER_IMAGE_NAME)
PROJECTNAME = $(addprefix ${GITBASEURL}/,${NAME})
COMMIT?=$(shell git rev-parse --short HEAD)
BUILD_TIME?=$(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
GOOS?=linux
GOARCH?=amd64
PORT?=8080
NS?=default

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# Setting SHELL to bash allows bash commands to be executed by recipes.

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk commands is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

fmt: ## Run go fmt against code.
	go fmt ./...

vet: ## Run go vet against code.
	go vet ./...

test: fmt vet ## Run tests.
	 go test ./...

run: fmt vet ## Run.
	 go run ./cmd/${NAME}.go

commithistory: ## create the commithistory in a nice format
	 git log --reverse > CommitHistory.txt

##@ Build

clean: ## remove previous binaries
	rm -f bin/${NAME}

build: clean ## build a version of the app, pass Buildversion, Comit and projectname as build arguments
	CGO_ENABLED=0 GOOS=${GOOS} GOARCH=${GOARCH} go build \
		-trimpath \
		-ldflags "-s -w -X ${PROJECTNAME}/pkg/version.Release=${VERSION} \
		-X ${PROJECTNAME}/pkg/version.Commit=${COMMIT} -X ${PROJECTNAME}/pkg/version.BuildTime=${BUILD_TIME}" \
		-o bin/${NAME} ./cmd/${NAME}.go

docker-build: ## Build the docker image and tag it with the current version and :latest
	docker build \
		-t ${IMAGE_REPOSITORY}:${VERSION} \
		-t ${IMAGE_REPOSITORY}:latest \
		--build-arg VERSION=${VERSION} \
		--build-arg COMMIT=${COMMIT} \
		--build-arg BUILD_TIME=${BUILD_TIME} \
		. -f ./dockerfiles/Dockerfile

docker-run: docker-build ## Build the docker image and tag it and run it in docker
	docker stop ${NAME} || true && docker rm ${NAME} || true
	docker run --name ${NAME} -p ${PORT}:8080 --rm \
		${IMAGE_REPOSITORY}:${VERSION}

docker-push: ##push your image to the docker hub
	docker push ${IMAGE_REPOSITORY}:${VERSION}
	docker push ${IMAGE_REPOSITORY}:latest

##@ Kubernetes

kustomize-manifests: ## Refresh the base Deployment and each overlay Service from templates.
	cp ./kube-manifests/templates/01-deployment.yml ./kube-manifests/kustomize/base/01-deployment.yml
	cp ./kube-manifests/templates/02-nodeport-service.yml ./kube-manifests/kustomize/overlays/dev/02-nodeport-service.yml
	cp ./kube-manifests/templates/03-loadbalancer-service.yml ./kube-manifests/kustomize/overlays/prod/03-loadbalancer-service.yml
	find ./kube-manifests/kustomize/ \( -name '*.yml' \) -maxdepth 5 -exec sed -i 's|{{.IMAGE_REPOSITORY}}|$(IMAGE_REPOSITORY)|g' {} \;
	find ./kube-manifests/kustomize/ \( -name '*.yml' \) -maxdepth 5 -exec sed -i 's/{{.APP_NAME}}/$(NAME)/g' {} \;
	find ./kube-manifests/kustomize/ \( -name '*.yml' \) -maxdepth 5 -exec sed -i 's/{{.VERSION}}/$(VERSION)/g' {} \;

kube-manifests: kube-clean ## generated the kubernetes manifests and replaces variables in them
	cp ./kube-manifests/templates/*.yml ./kube-manifests/release
	find ./kube-manifests/release/ \( -name '*.yml' \) -maxdepth 4 -exec sed -i 's|{{.IMAGE_REPOSITORY}}|$(IMAGE_REPOSITORY)|g' {} \;
	find ./kube-manifests/release/ \( -name '*.yml' \) -maxdepth 4 -exec sed -i 's/{{.APP_NAME}}/$(NAME)/g' {} \;
	find ./kube-manifests/release/ \( -name '*.yml' \) -maxdepth 4 -exec sed -i 's/{{.VERSION}}/$(VERSION)/g' {} \;

kube-clean: ## removes release manifests
	rm -f ./kube-manifests/release/*.yml 

kube-apply-nodeport: ## apply kube manifests
	kubectl apply -f kube-manifests/release/01-deployment.yml -n ${NS}
	kubectl apply -f kube-manifests/release/02-nodeport-service.yml -n ${NS}

kube-apply-lb: ## apply kube manifests
	kubectl apply -f kube-manifests/release/01-deployment.yml -n ${NS}
	kubectl apply -f kube-manifests/release/03-loadbalancer-service.yml -n ${NS}

kube-apply: kube-apply-lb ## Compatibility alias: apply the LoadBalancer release.

kube-remove: ## remove kube manifests
	kubectl delete -f kube-manifests/release -n ${NS} --ignore-not-found=true

kube-ns: ## create desired namespace
	kubectl create namespace ${NS}

kube-removens: ## remove the desired namespace
	kubectl delete namespace ${NS}

kube-renew: docker-build kube-manifests kube-remove kube-apply ## Build, replace, and deploy the LoadBalancer release.
