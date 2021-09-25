# VERSION defines the project version for the bundle.
# Update this value when you upgrade the version of your project.
VERSION ?= 0.0.1
NAME = $(shell basename "`pwd`")
GITBASEURL = github.com/jhoelzel
PROJECTNAME = $(addprefix ${GITBASEURL}/,${NAME})
IMAGE_NAME=${NAME}:${VERSION}
IMAGE_NAME_LATEST=${NAME}:latest
COMMIT?=$(shell git rev-parse --short HEAD)
BUILD_TIME?=$(shell date -u '+%Y-%m-%d_%H:%M:%S')
TIMEZONE="Europe/Berlin"
GOOS?=linux
GOARCH?=amd64
PORT?=80
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
		-ldflags "-s -w -X ${PROJECTNAME}/pkg/version.Release=${VERSION} \
		-X ${PROJECTNAME}/pkg/version.Commit=${COMMIT} -X ${PROJECTNAME}/pkg/version.BuildTime=${BUILD_TIME}" \
		-o bin/${NAME} ./cmd/${NAME}.go

docker-build: build ## Build the docker image and tag it with the current version and :latest
	sudo docker build -t ${IMAGE_NAME} -t ${IMAGE_NAME_LATEST} --build-arg tz=${TIMEZONE} . -f ./dockerfiles/Dockerfile

docker-run: docker-build ## Build the docker image and tag it and run it in docker
	sudo docker stop $(NAME):$(VERSION) || true && sudo docker rm $(NAME):$(VERSION) || true
	sudo docker run --name ${NAME} -p ${PORT}:${PORT} --rm \
		-e "PORT=${PORT}" \
		$(NAME):$(VERSION)

##@ Kubernetes

kube-manifests: kube-clean ## generated the kubernetes manifests and replaces variables in them
	cp ./kube-manifests/templates/*.yml ./kube-manifests/release/
	find ./kube-manifests/release/ \( -name '*.yml' \) -maxdepth 1 -exec sed -i 's/{{.IMAGE_NAME}}/$(IMAGE_NAME)/g' {} \;
	find ./kube-manifests/release/ \( -name '*.yml' \) -maxdepth 1 -exec sed -i 's/{{.APP_NAME}}/$(NAME)/g' {} \;
	find ./kube-manifests/release/ \( -name '*.yml' \) -maxdepth 1 -exec sed -i 's/{{.VERSION}}/$(VERSION)/g' {} \;
	find ./kube-manifests/release/ \( -name '*.yml' \) -maxdepth 1 -exec sed -i 's/{{.BUILD_TIME}}/$(BUILD_TIME)/g' {} \;
	find ./kube-manifests/release/ \( -name '*.yml' \) -maxdepth 1 -exec sed -i 's/{{.BUILDVERSION}}/$(VERSION)/g' {} \;

kube-clean: ## removes release manifests
	rm -f ./kube-manifests/release/*.yml 

kube-apply: ## apply kube manifests
	kubectl apply -f kube-manifests/release -n ${NS}

kube-remove: ## remove kube manifests
	kubectl delete -f kube-manifests/release -n ${NS}

kube-ns: ## create desired namespace
	kubectl create namespace ${NS}

kube-removens: ## remove the desired namespace
	kubectl delete namespace ${NS}

kube-renew: build docker-build kube-remove kube-apply ## build, docker-build, remove existing deployment, deploy

