# Build the viewer as a static binary, then ship it on scratch.
# Result is ~7MB with no OS, no shell, no interpreter, and nothing to patch.
#
# BUILDER is overridable because the alpine variant is not reachable from every
# network; any golang image works:
#   docker build --build-arg BUILDER=golang:latest -t newapi-logviewer .
ARG BUILDER=golang:1.24-alpine
FROM ${BUILDER} AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ui.html ./
COPY assets ./assets
# Tests are excluded from the image by .dockerignore, so build only.
# CGO off => no libc dependency, so the binary runs on an empty filesystem.
# -s -w strips the symbol table and DWARF; this is not a debuggable target.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /logviewer .

FROM scratch
COPY --from=build /logviewer /logviewer
# new-api is normally reached over plain HTTP inside a compose network, so no
# CA bundle is shipped. If NEWAPI_URL is https, add:
#   COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
USER 65534:65534
EXPOSE 7070
ENTRYPOINT ["/logviewer"]
