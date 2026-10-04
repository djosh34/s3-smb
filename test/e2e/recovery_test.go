// SPDX-License-Identifier: AGPL-3.0-only
// Package e2e tests the foreground executable over real signed SMB and MinIO.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
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
	"github.com/djosh34/s3-smb/internal/backup"
	smb "github.com/hirochachacha/go-smb2"
	"golang.org/x/sys/unix"
)

const password = "smb-e2e-secret-marker"
const passphrase = "encryption-e2e-secret-marker-with-entropy"

type fixture struct {
	t                            *testing.T
	bucket, root, addr, endpoint string
	clientAddr                   string // Empty uses the daemon address; chaos tests set a proxy address.
	encrypted                    bool
	password, secret             string
	binary                       string // Daemon selected when the fixture is created.
	cacheSize                    string
	storageCapacity              string        // Empty means no volume limit.
	interval                     string        // Metadata backup interval. Empty means 2s.
	startupTimeout               time.Duration // Zero means 45s.
	readonly, failStart          bool
	store                        *s3.Client
	generation                   int
}
type daemon struct {
	cmd     *exec.Cmd
	done    chan error
	tty     *os.File
	logs    []*os.File
	stopped bool
	t       *testing.T
}

func newFixture(t *testing.T, encrypted bool) *fixture {
	t.Helper()
	return newFixtureWithBinary(t, encrypted, os.Getenv("S3_SMB_E2E_BINARY"))
}

func newFixtureWithBinary(t *testing.T, encrypted bool, binary string) *fixture {
	t.Helper()
	endpoint := os.Getenv("S3_SMB_E2E_ENDPOINT")
	if endpoint == "" {
		t.Skip("needs MinIO: run scripts/check.sh")
	}
	if binary == "" {
		t.Fatal("test daemon binary is not set")
	}
	f := &fixture{t: t, binary: binary, endpoint: endpoint, encrypted: encrypted, password: password, secret: passphrase, bucket: fmt.Sprintf("smb-e2e-%d", time.Now().UnixNano())}
	f.store = s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String(endpoint), UsePathStyle: true, Credentials: credentials.NewStaticCredentialsProvider("s3smb-test-access", "s3smb-test-secret-only", "")})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := f.store.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(f.bucket)}); err != nil {
		t.Fatal(err)
	}
	f.freshLocal()
	return f
}
func (f *fixture) freshLocal() {
	f.t.Helper()
	if f.root != "" {
		if err := os.RemoveAll(f.root); err != nil {
			f.t.Fatal(err)
		}
	}
	var err error
	f.root, err = os.MkdirTemp("", "s3-smb-local-")
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { os.RemoveAll(f.root) })
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		f.t.Fatal(err)
	}
	f.addr = l.Addr().String()
	l.Close()
}
func (f *fixture) config() string {
	key := fmt.Sprintf("{value: %q}", f.secret)
	capacity := f.cacheSize
	if capacity == "" {
		capacity = "0 MB"
	}
	storageSettings := ""
	if f.storageCapacity != "" {
		storageSettings = fmt.Sprintf("  capacity: %q\n", f.storageCapacity)
	}
	interval := f.interval
	if interval == "" {
		interval = "2s"
	}
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
  access_key: {value: s3smb-test-access}
  secret_key: {value: s3smb-test-secret-only}
encryption:
  enabled: %t
  passphrase: %s
backup:
  interval: %s
  trash_days: 14
logging:
  format: json
  level: info
`, f.addr, f.password, f.readonly, storageSettings, capacity, f.bucket, f.endpoint, f.encrypted, key, interval)
}
func (f *fixture) start() *daemon {
	f.t.Helper()
	f.generation++
	config := filepath.Join(f.root, "config.yaml")
	if err := os.WriteFile(config, []byte(f.config()), 0600); err != nil {
		f.t.Fatal(err)
	}
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0600)
	if err != nil {
		f.t.Fatal(err)
	}
	if err = unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		f.t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		f.t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0600)
	if err != nil {
		master.Close()
		f.t.Fatal(err)
	}
	artifacts := os.Getenv("S3_SMB_TEST_ARTIFACTS")
	if artifacts == "" {
		artifacts = f.t.TempDir()
	}
	dir := filepath.Join(artifacts, strings.ReplaceAll(f.t.Name(), "/", "-"), fmt.Sprint(f.generation))
	if err = os.MkdirAll(dir, 0700); err != nil {
		f.t.Fatal(err)
	}
	d := &daemon{cmd: exec.Command(f.binary, "serve", "-c", config), done: make(chan error, 1), tty: master, t: f.t}
	d.cmd.Stdin = slave
	d.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	for _, stream := range []string{"stdout", "stderr"} {
		out, e := os.Create(filepath.Join(dir, stream+".log"))
		if e != nil {
			f.t.Fatal(e)
		}
		d.logs = append(d.logs, out)
		if stream == "stdout" {
			d.cmd.Stdout = out
		} else {
			d.cmd.Stderr = out
		}
	}
	if err = d.cmd.Start(); err != nil {
		f.t.Fatal(err)
	}
	slave.Close()
	go func() { d.done <- d.cmd.Wait() }()
	go func() {
		out, _ := os.Create(filepath.Join(dir, "prompts.log"))
		if out != nil {
			defer out.Close()
		}
		buf := make([]byte, 1024)
		var pending string
		for {
			n, err := master.Read(buf)
			if n > 0 {
				if out != nil {
					out.Write(buf[:n])
				}
				pending += string(buf[:n])
				if strings.Contains(pending, "Continue? [yes/no]:") {
					master.Write([]byte("yes\n"))
					pending = ""
				}
			}
			if err != nil {
				return
			}
		}
	}()
	f.t.Cleanup(func() { d.stop() })
	timeout := f.startupTimeout
	if timeout == 0 {
		timeout = 45 * time.Second
	}
	deadline := time.After(timeout)
	for {
		select {
		case err := <-d.done:
			d.stopped = true
			d.closeLogs()
			if f.failStart && err != nil {
				return d
			}
			f.t.Fatalf("unexpected daemon startup exit: %v; logs %s", err, dir)
		case <-deadline:
			d.cmd.Process.Kill()
			f.t.Fatalf("SMB startup timeout; logs %s", dir)
		default:
			conn, err := net.DialTimeout("tcp", f.addr, 100*time.Millisecond)
			if err == nil {
				conn.Close()
				if f.failStart {
					d.stop()
					f.t.Fatal("unsafe startup unexpectedly reached SMB readiness")
				}
				return d
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}
func (d *daemon) stop() {
	d.t.Helper()
	if d.stopped {
		return
	}
	d.stopped = true
	d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-d.done:
		if err != nil {
			d.t.Errorf("daemon shutdown: %v", err)
		}
	case <-time.After(35 * time.Second):
		d.cmd.Process.Kill()
		<-d.done
		d.t.Error("daemon exceeded bounded shutdown")
	}
	d.closeLogs()
}
func (d *daemon) closeLogs() {
	d.tty.Close()
	for _, f := range d.logs {
		f.Close()
		checkDaemonLog(d.t, f.Name())
	}
}
func checkDaemonLog(t *testing.T, path string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Error(err)
		return
	}
	defer file.Close()
	scan := bufio.NewScanner(file)
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
		for _, marker := range []string{password, passphrase, "wrong-encryption-passphrase-e2e-marker", "s3smb-test-secret-only", "s3smb-test-access"} {
			if bytes.Contains(scan.Bytes(), []byte(marker)) {
				t.Errorf("secret marker leaked in %s line %d", path, line)
			}
		}
	}
	if err := scan.Err(); err != nil {
		t.Error(err)
	}
}
func (f *fixture) connectAddr() string {
	if f.clientAddr != "" {
		return f.clientAddr
	}
	return f.addr
}

func (f *fixture) connect(user, password string) (*smb.Share, func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	conn, err := net.DialTimeout("tcp", f.connectAddr(), time.Second)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	dial := smb.Dialer{Negotiator: smb.Negotiator{RequireMessageSigning: true}, Initiator: &smb.NTLMInitiator{User: user, Password: password}}
	session, err := dial.DialContext(ctx, conn)
	if err != nil {
		conn.Close()
		cancel()
		return nil, nil, err
	}
	share, err := session.Mount("TimeMachine")
	if err != nil {
		session.Logoff()
		conn.Close()
		cancel()
		return nil, nil, err
	}
	return share.WithContext(ctx), func() { share.Umount(); session.Logoff(); conn.Close(); cancel() }, nil
}
func (f *fixture) share() (*smb.Share, func()) {
	f.t.Helper()
	s, close, err := f.connect("backup", f.password)
	if err != nil {
		f.t.Fatal(err)
	}
	return s, close
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
	if err = file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
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
			t.Fatalf("full SHA256 mismatch %s: got %x want %x", name, sha256.Sum256(got), sha256.Sum256(want))
		}
	}
}
func (f *fixture) protectedAfter(after time.Time) {
	f.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(f.root, "state", "backup-receipt.json"))
		if err == nil {
			var receipt backup.Receipt
			if json.Unmarshal(data, &receipt) == nil && receipt.Snapshot.After(after) {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatal("no successful durable metadata receipt after final SMB flush")
}
func TestSMBToS3Smoke(t *testing.T) {
	f := newFixture(t, false)
	f.cacheSize = "8 MB"
	d := f.start()
	share, close := f.share()
	data := []byte("real signed SMB -> native JuiceFS -> MinIO\n")
	writeFile(t, share, "smoke.txt", data)
	verifyFiles(t, share, map[string][]byte{"smoke.txt": data})
	close()
	d.stop()
	d = f.start()
	share, close = f.share()
	verifyFiles(t, share, map[string][]byte{"smoke.txt": data})
	close()
	d.stop()
}
func TestRecovery(t *testing.T) {
	for _, encrypted := range []bool{false, true} {
		name := "plaintext"
		if encrypted {
			name = "encrypted"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, encrypted)
			d := f.start()
			s, close := f.share()
			large := make([]byte, 5_000_003)
			for i := range large {
				large[i] = byte((i*31 + i/257) % 251)
			}
			files := map[string][]byte{"empty": {}, "unicode-文件.txt": []byte("all expected files are checked\n"), "large.bin": large}
			for name, data := range files {
				writeFile(t, s, name, data)
			}
			verifyFiles(t, s, files)
			close()
			f.protectedAfter(time.Now())
			d.stop()
			// Remove ALL original local config/state/cache/keys, not only the SQLite DB.
			f.freshLocal()
			d = f.start()
			s, close = f.share()
			verifyFiles(t, s, files)
			files["resumed.txt"] = []byte("writes after fresh-install recovery\n")
			writeFile(t, s, "resumed.txt", files["resumed.txt"])
			close()
			f.protectedAfter(time.Now())
			d.stop()
			f.freshLocal()
			d = f.start()
			s, close = f.share()
			verifyFiles(t, s, files)
			close()
			d.stop()
			// Inspect the remote snapshot: plaintext gzip only when encryption is disabled.
			objects, err := f.store.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{Bucket: aws.String(f.bucket)})
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, obj := range objects.Contents {
				if strings.Contains(aws.ToString(obj.Key), "meta/snapshot-") {
					count++
					o, e := f.store.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(f.bucket), Key: obj.Key})
					if e != nil {
						t.Fatal(e)
					}
					header := make([]byte, 2)
					_, e = io.ReadFull(o.Body, header)
					o.Body.Close()
					if e != nil {
						t.Fatal(e)
					}
					gzip := bytes.Equal(header, []byte{0x1f, 0x8b})
					if gzip == encrypted {
						t.Fatal("remote metadata encryption mode mismatch")
					}
				}
			}
			if count == 0 {
				t.Fatal("no native metadata objects found")
			}
		})
	}
}
func TestAuthentication(t *testing.T) {
	f := newFixture(t, false)
	d := f.start()
	s, close := f.share()
	writeFile(t, s, "signed.txt", []byte("signed session"))
	close()
	for _, cred := range [][2]string{{"backup", "wrong-password"}, {"wrong-user", f.password}} {
		_, close, err := f.connect(cred[0], cred[1])
		if err == nil {
			close()
			t.Error("invalid SMB credentials authenticated")
		}
	}
	d.stop()
}
func TestReadOnly(t *testing.T) {
	f := newFixture(t, false)
	d := f.start()
	s, close := f.share()
	data := []byte("read-only fixture")
	writeFile(t, s, "keep.txt", data)
	close()
	d.stop()
	f.readonly = true
	d = f.start()
	s, close = f.share()
	verifyFiles(t, s, map[string][]byte{"keep.txt": data})
	if file, err := s.Create("forbidden.txt"); err == nil {
		file.Close()
		t.Error("read-only CREATE succeeded")
	}
	if err := s.Remove("keep.txt"); err == nil {
		t.Error("read-only DELETE succeeded")
	}
	close()
	d.stop()
}
