FROM ghcr.io/djosh34/minio:RELEASE.2025-04-22T22-12-26Z AS minio
FROM golang:1.26.3-bookworm@sha256:386d475a660466863d9f8c766fec64d7fdad3edac2c6a05020c09534d71edb4b
COPY --from=minio /usr/local/bin/minio /usr/local/bin/minio
RUN apt-get update && apt-get install -y --no-install-recommends samba-testsuite smbclient \
    && rm -rf /var/lib/apt/lists/*
ENV GOMAXPROCS=2 GOTOOLCHAIN=local CGO_ENABLED=1 GOFLAGS="-mod=readonly -p=2"
WORKDIR /src
