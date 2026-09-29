// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

const usage = `Usage: s3-smb [-c CONFIG] [--log-format text|json] serve
       s3-smb help
       s3-smb version

Foreground SMB service backed by S3. Configuration defaults to
$XDG_CONFIG_HOME/s3-smb/config.yaml or $HOME/.config/s3-smb/config.yaml.
-c and --log-format may appear before or after serve.
Public sizes use decimal MB (1,000,000 bytes) and GB (1,000,000,000 bytes).
`

type arguments struct{ command, configPath, logFormat string }

// logOverride is deliberately independent of parsing/configuration: even a
// malformed command or config must honor a valid explicit logging override.
func logOverride(args []string) string {
	format := ""
	for i, arg := range args {
		if strings.HasPrefix(arg, "--log-format=") {
			format = strings.TrimPrefix(arg, "--log-format=")
		}
		if arg == "--log-format" && i+1 < len(args) {
			format = args[i+1]
		}
	}
	if format == "json" || format == "text" {
		return format
	}
	return ""
}

func parseArguments(args []string) (arguments, error) {
	var a arguments
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			a.command = "help"
			return a, nil
		case arg == "--version":
			a.command = "version"
			return a, nil
		case arg == "-c" || arg == "--log-format":
			i++
			if i == len(args) {
				return a, errors.New("option requires a value")
			}
			if arg == "-c" {
				a.configPath = args[i]
			} else {
				a.logFormat = args[i]
			}
		case strings.HasPrefix(arg, "-c="):
			a.configPath = strings.TrimPrefix(arg, "-c=")
		case strings.HasPrefix(arg, "--log-format="):
			a.logFormat = strings.TrimPrefix(arg, "--log-format=")
		case arg == "serve" || arg == "help" || arg == "version":
			if a.command != "" {
				return a, errors.New("only one command is allowed")
			}
			a.command = arg
		default:
			return a, errors.New("unknown command or option; use --help")
		}
	}
	if a.command == "" {
		return a, errors.New("a command is required; use --help")
	}
	if a.logFormat != "" && a.logFormat != "text" && a.logFormat != "json" {
		return a, errors.New("log format must be text or json")
	}
	return a, nil
}

func printHelp(w io.Writer) { _, _ = fmt.Fprint(w, usage) }
