// SPDX-License-Identifier: AGPL-3.0-only
// fixture creates and lists the MinIO bucket of the Mac acceptance test.
package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		// Do not print SDK/transport errors: they can contain signed URLs.
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: fixture bucket-create|bucket-list [flags]")
	}
	switch args[0] {
	case "bucket-create", "bucket-list":
		return bucketCommand(args[0], args[1:], out)
	default:
		return errors.New("unknown fixture subcommand")
	}
}

func loopbackAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("address must use a literal loopback IP and port")
	}
	return nil
}

func loopbackURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("endpoint must be a plain task-private HTTP loopback origin")
	}
	if err := loopbackAddress(u.Host); err != nil {
		return nil, err
	}
	return u, nil
}
