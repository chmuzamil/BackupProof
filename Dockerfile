# syntax=docker/dockerfile:1
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/backupproof ./cmd/backupproof
# Agent binaries for every platform, served to the one-line installers.
RUN for p in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
      os=${p%/*}; arch=${p#*/}; ext=; [ "$os" = windows ] && ext=.exe; \
      CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags="-s -w" \
        -o /out/downloads/backupproof-$os-$arch$ext ./cmd/backupproof; \
    done

# Control plane: static binary, no shell, runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot AS server
COPY --from=build /out/backupproof /usr/local/bin/backupproof
COPY --from=build /out/downloads /opt/backupproof/downloads
VOLUME /data
EXPOSE 8420
ENV BP_DATA=/data BP_LISTEN=:8420 BP_DOWNLOADS=/opt/backupproof/downloads
# The built-in agent protects paths mounted into this container (e.g. /host).
ENTRYPOINT ["backupproof", "server"]

# Agent: includes database client tools and the docker CLI for restore tests.
FROM debian:bookworm-slim AS agent
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates postgresql-client mariadb-client docker.io \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/backupproof /usr/local/bin/backupproof
ENV BP_STATE=/var/lib/backupproof
VOLUME /var/lib/backupproof
ENTRYPOINT ["backupproof", "agent"]
CMD ["run"]
