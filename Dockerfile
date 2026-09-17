# Stage 1: Build static binary
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

WORKDIR /src
RUN apk add --no-cache ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMPRESS=0
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /bin/omnigo . && \
    if [ "$COMPRESS" = "1" ] || [ "$COMPRESS" = "true" ]; then \
        apk add --no-cache upx && upx --best --lzma /bin/omnigo; \
    fi

# Assemble the runtime rootfs. Copying a single parent directory preserves the
# nested directory modes and ownership below it; copying a directory to a
# destination path normalizes that destination to 0755 root:root.
RUN set -eux; \
    mkdir -p /rootfs/usr/local/bin /rootfs/etc/ssl/certs /rootfs/tmp /rootfs/home/omnigo/.config/omnigo; \
    cp /bin/omnigo /rootfs/usr/local/bin/omnigo; \
    cp /etc/ssl/certs/ca-certificates.crt /rootfs/etc/ssl/certs/ca-certificates.crt; \
    printf 'omnigo:x:1000:1000:omnigo:/home/omnigo:/sbin/nologin\n' > /rootfs/etc/passwd; \
    printf 'omnigo:x:1000:\n' > /rootfs/etc/group; \
    chown -R 1000:1000 /rootfs/home; \
    chmod 1777 /rootfs/tmp

# Stage 2: Minimal scratch runtime
FROM scratch

COPY --from=builder /rootfs/ /

ENV HOME=/home/omnigo
USER 1000:1000
WORKDIR /home/omnigo

VOLUME ["/home/omnigo/.config/omnigo"]
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/omnigo"]
