// SPDX-License-Identifier: AGPL-3.0-only
// fixture provides task-private MinIO observation for the native Mac acceptance.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
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
		return errors.New("usage: fixture serve|bucket-create|bucket-list [flags]")
	}
	switch args[0] {
	case "serve":
		return serve(args[1:], out)
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

func serve(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	upstream := flags.String("upstream", "http://127.0.0.1:19000", "task-private MinIO origin")
	listen := flags.String("listen", "127.0.0.1:19001", "loopback S3 proxy")
	control := flags.String("control", "127.0.0.1:19002", "loopback controls")
	events := flags.String("events", "", "new private JSONL evidence file (must not exist)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *events == "" {
		return errors.New("serve requires --events PATH and no positional arguments")
	}
	target, err := loopbackURL(*upstream)
	if err != nil {
		return err
	}
	for _, addr := range []string{*listen, *control} {
		if err := loopbackAddress(addr); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(*events, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("cannot create new evidence file")
	}
	defer file.Close()
	p := newProxy(target, file)
	dataListener, err := net.Listen("tcp", *listen)
	if err != nil {
		return errors.New("cannot bind proxy loopback listener")
	}
	defer dataListener.Close()
	controlListener, err := net.Listen("tcp", *control)
	if err != nil {
		return errors.New("cannot bind control loopback listener")
	}
	defer controlListener.Close()
	dataServer := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	controlServer := &http.Server{Handler: p.control(), ReadHeaderTimeout: 5 * time.Second}
	defer dataServer.Close()
	defer controlServer.Close()
	defer p.release()
	done := make(chan error, 2)
	go func() { done <- dataServer.Serve(dataListener) }()
	go func() { done <- controlServer.Serve(controlListener) }()
	if err := json.NewEncoder(out).Encode(map[string]string{
		"proxy":   "http://" + dataListener.Addr().String(),
		"control": "http://" + controlListener.Addr().String(),
	}); err != nil {
		return errors.New("cannot report fixture listeners")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	select {
	case <-ctx.Done():
		return nil
	case <-p.failed:
		return errors.New("evidence write/sync failed; fixture stopped")
	case <-done:
		return errors.New("fixture listener stopped unexpectedly")
	}
}
