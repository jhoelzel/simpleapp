# Simpleapp

Simpleapp is a simple app that defines a basic kubernetes app i use in my trainings.
It contains a simple MVC structure for packages, a Mux subrouter integration, kube manifests and an easy to learn structure.

Its main goals are:

- Making kubernetes endpoints for your app is easy
- Automating the kubernetes manifests with a little help of sed and make it fun to work with it
- To teach the fact that deploying and testing can go hand in hand
- To teach how fast you can actually setup a golang application for kubernetes and deploy them

NOTE: I usually present this in combination with a fine tuned visual studio dev container. So this project assumes that you are already running on a linux host. If this is not the case be arware that there will be another build step for you neccesairy in the dockerfile. Another upside of a container based development environment is just that you can skip the buildimage inbetween.

## It Contains

- Gracefull shutdown
- Logging to SDTOUT
- MVC Oriented architechture with mux subrouter
- Kubernetes manifests with nodeport ingress labels and deployment
- Makefile for debugging testing and deploying in docker as well as kubernetes
- Stubs for Kubernetes readiness and health
- Image building, versioning and deploying
- Build tags that are read inside of the app so version and buildtime are available in the package
- Uses gcr.io/distroless/static:nonroot as docker base image
- Thefore the built container is 8 MB only
- TIMEZONE support

## Advantages

- With the proper use of the makefile you will be able to iterate your project quickly: all thats needed for public use is a push to your cotainer registry
- You can modify a container quickly by adding different static files into the static folder for proofs. (For instance for demoing going from v1 to v2)
- Minimal dependencies: the only thing required by this package is gorilla/mux

## Makefile instrunctions

HINT: The default namespace for Kubernetes is "default", you can change it by providing a NS argument:

make kube-apply  NS=foo applies the kubernetes manifests in the namespace foo. If your namespace does not exist make kube-ns  NS=foo will create it for you

```
Usage:
  make <target>

General
  help             Display this help.

Development
  fmt              Run go fmt against code.
  vet              Run go vet against code.
  test             Run tests.
  run              Run.
  commithistory    create the commithistory in a nice format

Build
  clean            remove previous binaries
  build            build a version of the app, pass Buildversion, Comit and projectname as build arguments
  docker-build     Build the docker image and tag it with the current version and :latest
  docker-run       Build the docker image and tag it and run it in docker

Kubernetes
  kube-manifests   generated the kubernetes manifests and replaces variables in them
  kube-clean       removes release manifests
  kube-apply       apply kube manifests
  kube-remove      remove kube manifests
  kube-renew       build, docker-build, remove existing deployment, deploy
```