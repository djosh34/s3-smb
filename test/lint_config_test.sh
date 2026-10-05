#!/usr/bin/env bash
# Check scope, suppression rules and formatting in a separate throwaway module.
set -Eeuo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
tools=$1
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
cp "$root/.golangci.yml" "$fixture/.golangci.yml"
printf 'module example.com/lintfixture\n\ngo 1.26.3\n' > "$fixture/go.mod"
cd "$fixture"
fail() { echo "Lint config test failed: $*" >&2; exit 1; }
lint_passes() {
  if ! "$tools/golangci-lint" run ./... > "$fixture/output" 2>&1; then
    while IFS= read -r line; do printf '%s\n' "$line" >&2; done < "$fixture/output"
    fail 'expected lint to pass'
  fi
}
lint_fails() {
  if "$tools/golangci-lint" run ./... > "$fixture/output" 2>&1; then
    fail 'expected lint to fail'
  fi
  for finding in "$@"; do
    grep -F "$finding" "$fixture/output" >/dev/null || fail "missing $finding"
  done
}
# Vendored trees are excluded recursively, even with deliberate errors.
for directory in internal/juicefs/probe internal/thirdparty/probe; do
  mkdir -p "$directory"
  cat > "$directory/probe.go" <<'GO'
package probe
import "os"
//nolint
func broken() { os.Chdir("."); panic("excluded") }
GO
done
lint_passes

# Our code, including the root package, gets the full config.
mkdir -p internal/smb
cat > internal/smb/probe.go <<'GO'
// Package smb tests the lint configuration.
package smb
import "os"
// Probe exercises error and panic checks.
func Probe() {
	os.Chdir(".")
	panic("new package")
}
//nolint
func marker() {}
GO
lint_fails '(errcheck)' '(forbidigo)' '(nolintlint)'
mv internal/smb internal/app
lint_fails '(errcheck)' '(forbidigo)' '(nolintlint)'
rm -rf internal/app
cat > main.go <<'GO'
// Package main tests the lint configuration.
package main

func main() { panic("root") }
GO
lint_fails '(forbidigo)'
rm main.go

mkdir -p internal/smb cmd/probe test/macos/fullsync
cat > internal/smb/print_test.go <<'GO'
package smb

import (
	output "fmt"
	"testing"
)

func TestPrint(t *testing.T) {
	if _, err := output.Println("allowed in tests"); err != nil {
		t.Fatal(err)
	}
}
GO
cat > cmd/probe/main.go <<'GO'
package main

import "os"

func main() {
	os.Exit(0)
}
GO
cp cmd/probe/main.go test/macos/fullsync/main.go
lint_passes
cat > test/macos/fullsync/write.go <<'GO'
package main

import "os"

func exitHelper() {
	os.Exit(0)
}
GO
lint_fails '(forbidigo)'
rm test/macos/fullsync/write.go
cat > internal/smb/main.go <<'GO'
package smb

import "os"

// Exit must not be allowed just because the file is named main.go.
func Exit() {
	os.Exit(0)
}
GO
lint_fails '(forbidigo)'
rm internal/smb/main.go

cat > internal/smb/errors.go <<'GO'
package smb

import "os"

// Ignore tests the required suppression syntax.
func Ignore() {
	//nolint:errcheck,gosec // The fixture deliberately ignores this error.
	os.Chdir(".")
}
GO
lint_passes
# A named suppression without a reason must still fail.
cat > internal/smb/errors.go <<'GO'
package smb

import "os"

// Ignore tests the required suppression syntax.
func Ignore() {
	//nolint:errcheck,gosec
	os.Chdir(".")
}
GO
lint_fails '(nolintlint)'
# Formatters are part of run, not a command that silently rewrites source.
printf 'package smb\n\nfunc formatted() {\n\n}\n' > internal/smb/format.go
lint_fails '(gofumpt)'
echo 'Lint config tests passed'
