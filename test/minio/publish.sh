#!/usr/bin/env bash
# Preflight for the MinIO publishing workflow. Run from the repository root.
set -Eeuo pipefail
: "${GITHUB_OUTPUT:?}"
release=$(awk -F= '$1 == "ARG MINIO_RELEASE" {print $2}' test/Dockerfile)
commit=$(awk -F= '$1 == "ARG MINIO_COMMIT" {print $2}' test/Dockerfile)
[[ $release =~ ^RELEASE\.[0-9TZ-]+$ ]]
[[ $commit =~ ^[0-9a-f]{40}$ ]]
printf 'release=%s\ncommit=%s\n' "$release" "$commit" >> "$GITHUB_OUTPUT"

# Dockerfile edits that leave the source pin unchanged do not publish an image.
if [[ ${EVENT:-} == push ]]; then
  : "${BEFORE:?}"
  git fetch --depth=1 origin "$BEFORE"
  previous=$(git show "$BEFORE:test/Dockerfile" | awk -F= '$1 == "ARG MINIO_COMMIT" {print $2}')
  if [[ $commit == "$previous" ]]; then
    echo 'MinIO commit is unchanged'
    echo 'publish=false' >> "$GITHUB_OUTPUT"
    exit 0
  fi
fi

# Only a confirmed 404 permits a push. Network and authorization errors stop it.
token=$(curl -fsS 'https://ghcr.io/token?service=ghcr.io&scope=repository:djosh34/minio:pull' \
  | python3 -c 'import json, sys; print(json.load(sys.stdin)["token"])')
status=$(curl -sS -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer $token" \
  -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json' \
  "https://ghcr.io/v2/djosh34/minio/manifests/$release")
case "$status" in
  200) echo "Refusing to overwrite $release"; echo 'publish=false' >> "$GITHUB_OUTPUT" ;;
  404) echo 'publish=true' >> "$GITHUB_OUTPUT" ;;
  *) echo "MinIO registry returned HTTP $status" >&2; exit 1 ;;
esac
