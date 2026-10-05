// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"errors"
	"strings"
)

const usage = `Usage: s3-smb [-c CONFIG] [--log-format text|json] serve
       s3-smb help
       s3-smb version

Foreground SMB service backed by S3. Configuration defaults to
$XDG_CONFIG_HOME/s3-smb/config.yaml or $HOME/.config/s3-smb/config.yaml.
-c and --log-format may appear before or after serve.
Public sizes use decimal MB (1,000,000 bytes) and GB (1,000,000,000 bytes).
Source: https://github.com/djosh34/s3-smb (AGPL-3.0-only; see NOTICE for upstream licenses)
`

type arguments struct{ command, configPath, logFormat string }

// logOverride finds the last --log-format value before the command line is
// parsed.
func logOverride(args []string) string {
	format := ""
	for i, arg := range args {
		if value, ok := strings.CutPrefix(arg, "--log-format="); ok {
			format = value
		}
		if arg == "--log-format" && i+1 < len(args) {
			format = args[i+1]
		}
	}
	return format
}

func parseArguments(args []string) (arguments, error) {
	var a arguments
	for len(args) > 0 {
		arg := args[0]
		args = args[1:]
		switch {
		case arg == "-h" || arg == "--help":
			a.command = "help"
			return a, nil
		case arg == "--version":
			a.command = "version"
			return a, nil
		case arg == "-c" || arg == "--log-format":
			if len(args) == 0 {
				return a, errors.New("option requires a value")
			}
			if arg == "-c" {
				a.configPath = args[0]
			} else {
				a.logFormat = args[0]
			}
			args = args[1:]
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
	return a, nil
}
