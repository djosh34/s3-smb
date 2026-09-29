// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"github.com/djosh34/s3-smb/internal/app"
	"os"
	"runtime/debug"
)

var version = "dev"

func main() { os.Exit(app.Main(os.Args[1:], buildVersion())) }

func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}
