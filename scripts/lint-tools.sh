#!/usr/bin/env bash
# Install the same lint tools locally and in CI. Print their directory.
set -Eeuo pipefail
case "$(uname -s)/$(uname -m)" in
  Linux/aarch64)
    arch=arm64 shell_arch=aarch64
    golangci_sha=ee7ec5f3453d15ddf106fae5a4d6c71737712348a979d1fe9cd52ec7ea299bae
    actionlint_sha=325e971b6ba9bfa504672e29be93c24981eeb1c07576d730e9f7c8805afff0c6
    shellcheck_sha=68a8133197a50beb8803f8d42f9908d1af1c5540d4bb05fdfca8c1fa47decefc
    ;;
  Linux/x86_64)
    arch=amd64 shell_arch=x86_64
    golangci_sha=ab90aeb7b066f92a33415b638a50fe5344bbb75a0d32ad30cc248d88f81032ab
    actionlint_sha=8aca8db96f1b94770f1b0d72b6dddcb1ebb8123cb3712530b08cc387b349a3d8
    shellcheck_sha=b7af85e41cc99489dcc21d66c6d5f3685138f06d34651e6d34b42ec6d54fe6f6
    ;;
  *) echo 'Lint tools need Linux ARM64 or AMD64' >&2; exit 1 ;;
esac
pin=$(sha256sum "$0")
cache=${XDG_CACHE_HOME:-$HOME/.cache}/s3-smb-lint/${pin%% *}-$arch
mkdir -p "$cache"
cache=$(cd "$cache" && pwd)
work=$(mktemp -d "$cache/install.XXXXXX")
trap 'rm -rf "$work"' EXIT
install_tool() {
  local name=$1 repo=$2 version=$3 archive=$4 checksum=$5 member=$6
  if [[ -x $cache/$name ]]; then return; fi
  curl --fail --location --retry 3 --silent --show-error \
    "https://github.com/$repo/releases/download/$version/$archive" -o "$work/archive"
  printf '%s  %s\n' "$checksum" "$work/archive" | sha256sum --check >&2
  tar -xf "$work/archive" -C "$work" "$member"
  chmod +x "$work/$member"
  # A rename publishes only a complete binary, even when two worktrees install it.
  mv -f "$work/$member" "$cache/$name"
}
install_tool golangci-lint golangci/golangci-lint v2.14.0 \
  "golangci-lint-2.14.0-linux-$arch.tar.gz" "$golangci_sha" \
  "golangci-lint-2.14.0-linux-$arch/golangci-lint"
install_tool actionlint rhysd/actionlint v1.7.12 \
  "actionlint_1.7.12_linux_$arch.tar.gz" "$actionlint_sha" actionlint
install_tool shellcheck koalaman/shellcheck v0.11.0 \
  "shellcheck-v0.11.0.linux.$shell_arch.tar.gz" "$shellcheck_sha" shellcheck-v0.11.0/shellcheck
printf '%s\n' "$cache"
