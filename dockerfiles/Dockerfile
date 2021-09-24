FROM gcr.io/distroless/static:nonroot
WORKDIR /
ADD ./bin/simpleapp /simpleapp
ADD ./dockerfiles/content /static

USER 65532:65532
ENTRYPOINT ["/simpleapp"]


