# One pinned toolchain and MinIO revision for local Linux and Linux CI.
FROM golang:1.26.3-bookworm@sha256:386d475a660466863d9f8c766fec64d7fdad3edac2c6a05020c09534d71edb4b AS fixture
ENV GOMAXPROCS=2 GOTOOLCHAIN=local CGO_ENABLED=1
# Published MinIO images are no longer reliably available. Build the immutable
# source for RELEASE.2025-04-22T22-12-26Z; never resolve a moving tag at test time.
RUN git init /minio && cd /minio && git remote add origin https://github.com/minio/minio.git \
    && git fetch --depth=1 origin 0d7408fc9969caf07de6a8c3a84f9fbb10a6739e \
    && git checkout --detach FETCH_HEAD \
    && go build -p 2 -trimpath -o /usr/local/bin/minio .

FROM golang:1.26.3-bookworm@sha256:386d475a660466863d9f8c766fec64d7fdad3edac2c6a05020c09534d71edb4b
COPY --from=fixture /usr/local/bin/minio /usr/local/bin/minio
ENV GOMAXPROCS=2 GOTOOLCHAIN=local CGO_ENABLED=1 GOFLAGS=-mod=readonly
WORKDIR /src
CMD ["bash", "/src/test/run-linux.sh"]
