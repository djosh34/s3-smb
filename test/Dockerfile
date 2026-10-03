ARG MINIO_RELEASE=RELEASE.2025-04-22T22-12-26Z
ARG MINIO_COMMIT=0d7408fc9969caf07de6a8c3a84f9fbb10a6739e
FROM ghcr.io/djosh34/minio:${MINIO_RELEASE}@sha256:ed94d551f2834fee8214847abf4090c0b24e3bbbfe79fb7a48ed712a61b8b882 AS minio
FROM golang:1.26.3-bookworm@sha256:386d475a660466863d9f8c766fec64d7fdad3edac2c6a05020c09534d71edb4b
COPY --from=minio /usr/local/bin/minio /usr/local/bin/minio
RUN apt-get update && apt-get install -y --no-install-recommends samba-testsuite smbclient \
    && rm -rf /var/lib/apt/lists/*
ENV GOMAXPROCS=2 GOTOOLCHAIN=local CGO_ENABLED=1 GOFLAGS="-mod=readonly -p=2"
WORKDIR /src
