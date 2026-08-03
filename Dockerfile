# Build both binaries: the server, and the manage CLI that owns migrations.
#
# The build stage runs on the BUILD platform and cross-compiles to the TARGET
# one. With CGO disabled that is free, and it means one image serves amd64 and
# arm64 without qemu emulation -- worth doing because the deploy host's
# architecture is not something to guess at.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/manage ./cmd/manage

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 stillworks
WORKDIR /app

COPY --from=build /out/server /out/manage /app/
# The settings loader walks up to this marker to find the project root.
COPY --chown=stillworks:stillworks .godjango /app/.godjango
COPY --chown=stillworks:stillworks entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

USER stillworks
EXPOSE 8000
ENV PORT=8000
ENTRYPOINT ["/app/entrypoint.sh"]
