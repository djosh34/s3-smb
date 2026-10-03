FROM golang:1.26.3-bookworm@sha256:386d475a660466863d9f8c766fec64d7fdad3edac2c6a05020c09534d71edb4b AS build
ARG MINIO_COMMIT
ENV GOMAXPROCS=2 GOTOOLCHAIN=local CGO_ENABLED=1
RUN test -n "$MINIO_COMMIT" \
    && git init /minio && cd /minio \
    && git remote add origin https://github.com/minio/minio.git \
    && git fetch --depth=1 origin "$MINIO_COMMIT" \
    && git checkout --detach FETCH_HEAD \
    && go build -p 2 -trimpath -o /usr/local/bin/minio .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /usr/local/bin/minio /usr/local/bin/minio
LABEL org.opencontainers.image.source="https://github.com/djosh34/s3-smb" \
      org.opencontainers.image.licenses="AGPL-3.0-only"
ENTRYPOINT ["/usr/local/bin/minio"]
