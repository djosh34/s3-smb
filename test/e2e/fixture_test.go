// SPDX-License-Identifier: AGPL-3.0-only

// Package e2e tests the s3-smb executable over real signed SMB and MinIO.
// scripts/check.sh runs it; without S3_SMB_E2E_ENDPOINT the tests skip.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smb "github.com/hirochachacha/go-smb2"
	"golang.org/x/sys/unix"

	"github.com/djosh34/s3-smb/internal/backup"
	"github.com/djosh34/s3-smb/internal/s3fault"
)

// Test secrets. The daemon logs must never contain them.
const (
	smbLogin     = "smb-e2e-secret-marker"                     // SMB password
	volumeKey    = "encryption-e2e-secret-marker-with-entropy" // encryption passphrase
	s3AccessKey  = "s3smb-test-access"
	s3SigningKey = "s3smb-test-secret-only" // S3 secret key
	// wrongVolumeKey is the passphrase of a test that expects recovery to fail.
	wrongVolumeKey = "wrong-encryption-passphrase-e2e-marker"
)

// macGUID is the client GUID of the one Mac the tests play.
var macGUID = [16]byte{0x4d, 0x61, 0x63}

// daemonBinary is the daemon under test, built by scripts/check.sh.
var daemonBinary = os.Getenv("S3_SMB_E2E_BINARY")

// fixture is one disposable bucket and one local install of the daemon.
type fixture struct {
	t     *testing.T
	store *s3.Client
	// logs holds each daemon run's logs, under S3_SMB_TEST_ARTIFACTS when set.
	logs                         *os.Root
	bucket, root, addr, endpoint string
	password, secret             string
	cacheSize                    string        // Empty means 0 MB.
	storageCapacity              string        // Empty means no volume limit.
	interval                     string        // Metadata backup interval. Empty means 2s.
	startupTimeout               time.Duration // Zero means 45s.
	generation                   int
	encrypted                    bool
	readonly                     bool
	// failStart expects the daemon to exit during startup.
	failStart bool
	// keepAlive starts the daemon again when it exits during startup, like
	// launchd KeepAlive, on the same address until the startup timeout.
	keepAlive bool
}

type daemon struct {
	t       *testing.T
	cmd     *exec.Cmd
	done    chan error
	tty     *os.File
	prompts chan error
	logs    *os.Root
	dir     string // Log directory, relative to logs.
	stopped bool
}

func newFixture(t *testing.T, encrypted bool) *fixture {
	t.Helper()
	endpoint := os.Getenv("S3_SMB_E2E_ENDPOINT")
	if endpoint == "" {
		t.Skip("needs MinIO: run scripts/check.sh")
	}
	if daemonBinary == "" {
		t.Fatal("S3_SMB_E2E_BINARY is not set: run scripts/check.sh")
	}
	artifacts := os.Getenv("S3_SMB_TEST_ARTIFACTS")
	if artifacts == "" {
		artifacts = t.TempDir()
	}
	logs, err := os.OpenRoot(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	closeOnCleanup(t, logs)
	f := &fixture{
		t: t, logs: logs, endpoint: endpoint, encrypted: encrypted, password: smbLogin, secret: volumeKey,
		bucket: fmt.Sprintf("smb-e2e-%d", time.Now().UnixNano()),
	}
	f.store = s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(endpoint), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(s3AccessKey, s3SigningKey, ""),
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := f.store.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(f.bucket)}); err != nil {
		t.Fatal(err)
	}
	f.freshLocal()
	return f
}

// closeOnCleanup closes c at the end of the test.
func closeOnCleanup(t *testing.T, c io.Closer) {
	t.Helper()
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
}

// newFaultProxy puts an S3 fault proxy between the daemon and MinIO. f.store
// still talks to MinIO directly.
func (f *fixture) newFaultProxy() *s3fault.Proxy {
	f.t.Helper()
	proxy, err := s3fault.New(f.t.Context(), f.endpoint)
	if err != nil {
		f.t.Fatal(err)
	}
	closeOnCleanup(f.t, proxy)
	f.endpoint = proxy.URL()
	return proxy
}

// freshLocal replaces all local config, state, cache and keys, keeping the
// bucket, and picks a new SMB port.
func (f *fixture) freshLocal() {
	f.t.Helper()
	if f.root != "" {
		if err := os.RemoveAll(f.root); err != nil {
			f.t.Fatal(err)
		}
	}
	f.root = f.t.TempDir()
	f.addr = freeAddress(f.t)
}

func freeAddress(t *testing.T) string {
	t.Helper()
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func (f *fixture) config() string {
	cacheSize := f.cacheSize
	if cacheSize == "" {
		cacheSize = "0 MB"
	}
	capacity := ""
	if f.storageCapacity != "" {
		capacity = fmt.Sprintf("  capacity: %q\n", f.storageCapacity)
	}
	interval := f.interval
	if interval == "" {
		interval = "2s"
	}
	key := fmt.Sprintf("{value: %q}", f.secret)
	if !f.encrypted {
		key = "{command: [/does-not-exist/encryption-disabled-must-not-execute]}"
	}
	return fmt.Sprintf(`smb:
  listen: %q
  share: TimeMachine
  username: backup
  password: %q
  read_only: %t
storage:
%s  state_dir: ./state
  cache_dir: ./cache
  cache_size: %q
s3:
  bucket: %q
  region: us-east-1
  endpoint: %q
  path_style: true
  access_key: {value: %s}
  secret_key: {value: %s}
encryption:
  enabled: %t
  passphrase: %s
backup:
  interval: %s
  trash_days: 14
logging:
  format: json
  level: info
`, f.addr, f.password, f.readonly, capacity, cacheSize, f.bucket, f.endpoint, s3AccessKey, s3SigningKey, f.encrypted, key, interval)
}

// logDir creates the log directory for the next daemon run and returns its
// path relative to f.logs.
func (f *fixture) logDir() string {
	f.t.Helper()
	f.generation++
	dir := filepath.Join(strings.ReplaceAll(f.t.Name(), "/", "-"), f.bucket, fmt.Sprint(f.generation))
	if err := f.logs.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

// start runs the daemon and waits until it serves SMB, answering "yes" to a
// recovery prompt. The port from freshLocal can be taken by another process
// before the daemon binds it; then start picks a new port, at most twice.
// With keepAlive it starts the daemon again instead.
func (f *fixture) start() *daemon {
	f.t.Helper()
	timeout := f.startupTimeout
	if timeout == 0 {
		timeout = 45 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for attempt := 1; ; attempt++ {
		d, again := f.startOnce(deadline)
		switch {
		case !again:
			return d
		case f.keepAlive:
			// launchd waits before it starts the daemon again.
			time.Sleep(time.Second)
		case attempt == 3:
			f.t.Fatalf("SMB port taken on every attempt; logs %s", d.path())
		default:
			f.addr = freeAddress(f.t)
		}
	}
}

// startOnce runs the daemon once and reports whether to start it again: the
// port was taken or, with keepAlive, the daemon exited during startup.
func (f *fixture) startOnce(deadline time.Time) (*daemon, bool) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.root, "config.yaml"), []byte(f.config()), 0o600); err != nil {
		f.t.Fatal(err)
	}
	master, slave := openPTY(f.t)
	d := &daemon{
		t: f.t, done: make(chan error, 1), tty: master, prompts: make(chan error, 1),
		logs: f.logs, dir: f.logDir(),
		cmd: exec.CommandContext(context.Background(), daemonBinary, "serve", "-c", "config.yaml"),
	}
	d.cmd.Dir = f.root
	d.cmd.Stdin = slave
	d.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	stdout, stderr, prompts := d.create("stdout.log"), d.create("stderr.log"), d.create("prompts.log")
	d.cmd.Stdout, d.cmd.Stderr = stdout, stderr
	err := d.cmd.Start()
	err = errors.Join(err, slave.Close())
	// The child holds its own copies of the log files.
	err = errors.Join(err, stdout.Close(), stderr.Close())
	if err != nil {
		f.t.Fatal(err)
	}
	go func() { d.done <- d.cmd.Wait() }()
	go func() { d.prompts <- errors.Join(answerPrompts(master, prompts), prompts.Close()) }()
	f.t.Cleanup(d.stop)

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-d.done:
			d.exited()
			if f.failStart && err != nil {
				return d, false
			}
			if f.keepAlive || bytes.Contains(d.read("stderr.log"), []byte(f.addr+": bind: address already in use")) {
				return d, true
			}
			f.t.Fatalf("daemon exited during startup: %v; logs %s", err, d.path())
		case <-timer.C:
			f.t.Fatalf("SMB startup timeout: %v; logs %s", d.kill(), d.path())
		case <-ticker.C:
			// Only the daemon's own log proves readiness; another process
			// may hold the port.
			if bytes.Contains(d.read("stderr.log"), []byte(`"msg":"SMB serving"`)) {
				if f.failStart {
					d.stop()
					f.t.Fatal("unsafe startup unexpectedly reached SMB readiness")
				}
				return d, false
			}
		}
	}
}

// openPTY returns a pseudo-terminal pair, so the daemon sees an interactive
// recovery prompt.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	err = unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0)
	var number int
	if err == nil {
		number, err = unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	}
	if err == nil {
		slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	}
	if err != nil {
		t.Fatal(errors.Join(err, master.Close()))
	}
	return master, slave
}

// answerPrompts copies the terminal output to log and answers each recovery
// prompt with "yes". It returns when the terminal closes.
func answerPrompts(tty *os.File, log *os.File) error {
	buf := make([]byte, 1024)
	var pending string
	for {
		n, err := tty.Read(buf)
		if n > 0 {
			if _, writeErr := log.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			pending += string(buf[:n])
			if strings.Contains(pending, "Continue? [yes/no]:") {
				if _, writeErr := tty.WriteString("yes\n"); writeErr != nil {
					return writeErr
				}
				pending = ""
			}
		}
		// EIO means the daemon closed its side; ErrClosed means closeLogs ran.
		if errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (d *daemon) path() string { return filepath.Join(d.logs.Name(), d.dir) }

func (d *daemon) create(name string) *os.File {
	d.t.Helper()
	file, err := d.logs.Create(filepath.Join(d.dir, name))
	if err != nil {
		d.t.Fatal(err)
	}
	return file
}

func (d *daemon) read(name string) []byte {
	d.t.Helper()
	data, err := d.logs.ReadFile(filepath.Join(d.dir, name))
	if err != nil {
		d.t.Fatal(err)
	}
	return data
}

// output returns everything the daemon wrote to stdout and stderr.
func (d *daemon) output() []byte {
	d.t.Helper()
	return append(d.read("stdout.log"), d.read("stderr.log")...)
}

// stop sends SIGTERM and expects a clean exit within the 30 second shutdown
// limit.
func (d *daemon) stop() {
	d.t.Helper()
	if d.stopped {
		return
	}
	if err := d.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		d.t.Error(err)
	}
	select {
	case err := <-d.done:
		d.exited()
		if err != nil {
			d.t.Errorf("daemon shutdown: %v", err)
		}
	case <-time.After(35 * time.Second):
		d.t.Errorf("daemon exceeded bounded shutdown: %v", d.kill())
	}
}

// running waits for duration and fails if the daemon exits meanwhile.
func (d *daemon) running(duration time.Duration) {
	d.t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case err := <-d.done:
		d.exited()
		d.t.Fatalf("daemon exited: %v", err)
	case <-timer.C:
	}
}

// alive fails the test if the daemon has exited.
func (d *daemon) alive() {
	d.t.Helper()
	select {
	case err := <-d.done:
		d.exited()
		d.t.Fatalf("daemon exited: %v", err)
	default:
	}
}

// kill sends SIGKILL, waits for the exit and returns the exit error.
func (d *daemon) kill() error {
	d.t.Helper()
	if err := d.cmd.Process.Kill(); err != nil {
		d.t.Fatal(err)
	}
	err := <-d.done
	d.exited()
	return err
}

// exited records that the daemon has exited and checks its logs.
func (d *daemon) exited() {
	d.t.Helper()
	d.stopped = true
	if err := d.tty.Close(); err != nil {
		d.t.Error(err)
	}
	if err := <-d.prompts; err != nil {
		d.t.Error(err)
	}
	for _, name := range []string{"stdout.log", "stderr.log"} {
		checkDaemonLog(d.t, filepath.Join(d.path(), name), d.read(name))
	}
}

// checkDaemonLog requires JSON lines without any test secret.
func checkDaemonLog(t *testing.T, path string, data []byte) {
	t.Helper()
	scan := bufio.NewScanner(bytes.NewReader(data))
	scan.Buffer(make([]byte, 4096), 2<<20)
	line := 0
	for scan.Scan() {
		line++
		if len(bytes.TrimSpace(scan.Bytes())) == 0 {
			continue
		}
		if !json.Valid(scan.Bytes()) {
			t.Errorf("non-JSON daemon output %s line %d", path, line)
		}
		for _, marker := range []string{smbLogin, volumeKey, wrongVolumeKey, s3SigningKey, s3AccessKey} {
			if bytes.Contains(scan.Bytes(), []byte(marker)) {
				t.Errorf("secret marker leaked in %s line %d", path, line)
			}
		}
	}
	if err := scan.Err(); err != nil {
		t.Error(err)
	}
}

// connect logs in and mounts the share. The connection closes at the end of
// the test if the returned function, which logs off, is not called, for
// example because the daemon was killed.
func (f *fixture) connect(user, password string) (*smb.Share, func(), error) {
	return f.connectAt(f.addr, user, password)
}

// connectAt connects like connect, to addr instead of the daemon's address.
func (f *fixture) connectAt(addr, user, password string) (*smb.Share, func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	f.t.Cleanup(func() {
		cancel()
		if closeErr := conn.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			f.t.Error(closeErr)
		}
	})
	dialer := smb.Dialer{
		// The new server lets one client in at a time, so every connection
		// uses the same client GUID, like the connections of one Mac.
		Negotiator: smb.Negotiator{RequireMessageSigning: true, ClientGuid: macGUID},
		Initiator:  &smb.NTLMInitiator{User: user, Password: password},
	}
	session, err := dialer.DialContext(ctx, conn)
	if err != nil {
		return nil, nil, err
	}
	share, err := session.Mount("TimeMachine")
	if err != nil {
		return nil, nil, errors.Join(err, session.Logoff())
	}
	disconnect := func() {
		// Logoff also closes the connection.
		if err := errors.Join(share.Umount(), session.Logoff()); err != nil {
			f.t.Error(err)
		}
	}
	return share.WithContext(ctx), disconnect, nil
}

// share connects as the configured user. The returned function disconnects.
func (f *fixture) share() (*smb.Share, func()) {
	f.t.Helper()
	share, disconnect, err := f.connect("backup", f.password)
	if err != nil {
		f.t.Fatal(err)
	}
	return share, disconnect
}

func writeFile(t *testing.T, share *smb.Share, name string, data []byte) {
	t.Helper()
	file, err := share.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := file.Write(data); err != nil || n != len(data) {
		t.Fatalf("write %s: %d/%d %v", name, n, len(data), err)
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		t.Fatal(err)
	}
}

func verifyFiles(t *testing.T, share *smb.Share, files map[string][]byte) {
	t.Helper()
	for name, want := range files {
		got, err := share.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if sha256.Sum256(got) != sha256.Sum256(want) {
			t.Fatalf("SHA256 mismatch %s: got %x want %x", name, sha256.Sum256(got), sha256.Sum256(want))
		}
	}
}

// receipt returns the receipt of the last metadata backup.
func (f *fixture) receipt() backup.Receipt {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, "state", "backup-receipt.json"))
	if err != nil {
		f.t.Fatal(err)
	}
	var receipt backup.Receipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		f.t.Fatal(err)
	}
	return receipt
}

// protectedAfter waits for a metadata backup taken after the given time.
func (f *fixture) protectedAfter(after time.Time) {
	f.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(f.root, "state", "backup-receipt.json"))
		var receipt backup.Receipt
		if err == nil && json.Unmarshal(data, &receipt) == nil && receipt.Snapshot.After(after) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatal("no metadata backup after the given time")
}
