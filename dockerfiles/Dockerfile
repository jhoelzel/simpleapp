FROM gcr.io/distroless/static:nonroot
WORKDIR /
ADD ./bin/simpleapp /simpleapp
ADD ./static /static
EXPOSE 8080

USER 65532:65532
ENTRYPOINT ["/simpleapp"]


