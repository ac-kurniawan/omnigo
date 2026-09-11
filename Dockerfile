# Stage 1: Build static binary
FROM golang:1.26-alpine AS builder

WORKDIR /src
RUN apk add --no-cache ca-certificates tzdata

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /bin/omnigo .

# Stage 2: Production runtime
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -g 1000 omnigo && \
    adduser -u 1000 -G omnigo -h /home/omnigo -D omnigo && \
    mkdir -p /home/omnigo/.config/omnigo && \
    chown -R omnigo:omnigo /home/omnigo

COPY --from=builder /bin/omnigo /usr/local/bin/omnigo

USER omnigo
WORKDIR /home/omnigo

VOLUME ["/home/omnigo/.config/omnigo"]
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/omnigo"]
