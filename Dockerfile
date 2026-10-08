# Build stage: full Go toolchain, discarded after the binaries are built.
FROM golang:1.27-alpine AS build
WORKDIR /src

# Dependencies first, in their own layer: editing code does not re-download modules.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO off gives static binaries that run on an image with no C library; -trimpath keeps local
# paths out of them, -s -w drops debug symbols.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/webhook-relay ./cmd/webhook-relay \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sink ./cmd/sink

# Runtime stage: no shell, no package manager, runs as a non-root user. Only the binaries and
# CA certificates (for https destinations) are in it.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/webhook-relay"]
