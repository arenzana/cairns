# Multi-stage: build both binaries once, ship a small runtime.
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO off so the binaries are static and the runtime stage stays minimal.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cairnsd ./cmd/cairnsd && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cairns-serve ./cmd/cairns-serve && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cairns-eval ./cmd/cairns-eval

FROM alpine:3.22
# ca-certificates is not optional: without it the TLS call to typesafe.ai fails
# with an opaque x509 error and reranking silently degrades to vector-only.
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 cairns
COPY --from=build /out/cairnsd /out/cairns-serve /out/cairns-eval /usr/local/bin/
USER cairns
# No ENTRYPOINT on purpose. This image carries three binaries and each service
# picks one with `command:`. With an ENTRYPOINT set, a compose `command` becomes
# ARGUMENTS to it, so the indexer silently ran the web server with the indexer
# path as an argv it ignored: two web servers, no indexing, no error.
CMD ["/usr/local/bin/cairns-serve"]
