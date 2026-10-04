// SPDX-License-Identifier: AGPL-3.0-only
package e2e

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/backup"
	"github.com/djosh34/s3-smb/internal/chaos"
	"github.com/djosh34/s3-smb/internal/netfault"
	"github.com/djosh34/s3-smb/internal/s3fault"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
)

func TestChaosS3Outage(t *testing.T) {
	seed := chaos.Seed(t)
	commands := []wire.Command{wire.Write, wire.Flush, wire.Read}
	chaos.Rand(seed, "outage-order").Shuffle(len(commands), func(i, j int) { commands[i], commands[j] = commands[j], commands[i] })
	for _, command := range commands {
		name := map[wire.Command]string{wire.Write: "write_through", wire.Flush: "flush", wire.Read: "cold_read"}[command]
		t.Run(name, func(t *testing.T) { runChaosOutage(t, seed, command, false) })
	}
	// Production retry budgets cannot be shortened through daemon config.
	// The gate observes storage exhaustion, not a client-side deadline.
	if os.Getenv("S3_SMB_CHECK_MODE") == "gate" {
		t.Run("beyond_limit", func(t *testing.T) { runChaosOutage(t, seed, wire.Read, true) })
	}
}

func runChaosOutage(t *testing.T, seed uint64, command wire.Command, permanent bool) {
	t.Helper()
	outage := 6*time.Second + time.Duration(chaos.Rand(seed, t.Name()).IntN(3))*time.Second
	if os.Getenv("S3_SMB_CHECK_MODE") == "gate" {
		outage = 300 * time.Second
	}
	bound := outage + 3*time.Minute
	if permanent {
		bound = 12 * time.Minute
	}
	// Replace this selector with newChaosFixture when the shared helper lands.
	binary := os.Getenv("S3_SMB_CHAOS_BINARY")
	if binary == "" {
		t.Skip("S3_SMB_CHAOS_BINARY is not set")
	}
	f := newFixture(t, false)
	f.binary = binary
	f.cacheSize = "0 MB"
	f.interval = "1h"
	metadata := command == wire.Flush
	if metadata {
		f.interval = (outage + 30*time.Second).String()
	}
	proxy := newFaultProxy(t, f.endpoint)
	f.endpoint = proxy.URL()
	d := startOutageDaemon(t, f)
	ledger := chaos.NewLedger()
	const path = "backup-band"
	data := make([]byte, 32<<10)
	random := chaos.Rand(seed, t.Name()+"-bytes")
	for i := range data {
		data[i] = byte(random.IntN(256))
	}
	if command == wire.Read {
		share, disconnect := f.share()
		ledger.Attempt(path, 0, data)
		writeFile(t, share, path, data)
		ledger.Write(path, 0, data)
		ledger.Flush(path)
		disconnect()
		d.stop()
		// Disable the chunk cache and restart to discard VFS read pages too.
		d = startOutageDaemon(t, f)
	}
	network, err := netfault.New(t.Context(), f.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := network.Close(); err != nil {
			t.Error(err)
		}
	})
	f.clientAddr = network.Address()
	ctx, cancel := context.WithTimeout(t.Context(), bound+outage+time.Minute)
	defer cancel()
	client, session := outageClient(t, ctx, f)
	next := session.NextMessageID
	message := func(command wire.Command, body []byte) wire.Message {
		request := wire.Message{Header: wire.Header{Command: command, MessageID: next, SessionID: session.SessionID, TreeID: session.TreeID, CreditCharge: 1, Credit: 1}, Body: body}
		next++
		return request
	}
	body, err := wire.EncodeCreateRequest(wire.CreateRequest{Name: path, DesiredAccess: 3, ShareAccess: 7, Disposition: 3, ImpersonationLevel: 2})
	if err != nil {
		t.Fatal(err)
	}
	created := outageExchange(t, ctx, client, message(wire.Create, body))
	if created.Header.Status != smb.StatusSuccess {
		t.Fatalf("CREATE: %+v", created.Header)
	}
	open, err := wire.DecodeCreateResponse(created)
	if err != nil {
		t.Fatal(err)
	}
	writeBody, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: open.ID, Data: data, Flags: 1})
	if err != nil {
		t.Fatal(err)
	}
	body = writeBody
	method := http.MethodPut
	switch command {
	case wire.Flush:
		body, err = wire.EncodeFlushRequest(wire.FlushRequest{ID: open.ID})
	case wire.Read:
		body, err = wire.EncodeReadRequest(wire.ReadRequest{ID: open.ID, Length: uint32(len(data))})
		method = http.MethodGet
	}
	if err != nil {
		t.Fatal(err)
	}
	var baseline backup.Receipt
	if metadata {
		baseline = outageReceipt(t, f)
		interval, err := time.ParseDuration(f.interval)
		if err != nil {
			t.Fatal(err)
		}
		wait := time.NewTimer(time.Until(baseline.Snapshot.Add(interval - time.Second)))
		defer wait.Stop()
		select {
		case <-wait.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	schedule := chaos.Schedule{{S3Outage: outage}}
	if permanent {
		schedule = chaos.Schedule{{S3: &s3fault.Fault{Status: http.StatusServiceUnavailable}}}
	}
	t.Logf("outage schedule:\n%s", schedule)
	t.Cleanup(proxy.RestoreS3)
	t.Cleanup(func() {
		if err := proxy.SetFault(s3fault.Fault{}); err != nil {
			t.Error(err)
		}
	})
	if command == wire.Flush {
		buffered, err := wire.EncodeWriteRequest(wire.WriteRequest{ID: open.ID, Data: data})
		if err != nil {
			t.Fatal(err)
		}
		ledger.Attempt(path, 0, data)
		assertOutageSuccess(t, wire.Write, outageExchange(t, ctx, client, message(wire.Write, buffered)), data)
		ledger.Write(path, 0, data)
	}
	start := time.Now()
	if err := schedule.Run(ctx, network, proxy); err != nil {
		t.Fatal(err)
	}
	request := message(command, body)
	if command == wire.Write {
		ledger.Attempt(path, 0, data)
	}
	if err := client.Send(ctx, []wire.Message{request}); err != nil {
		t.Fatal(err)
	}
	pendingCtx, pendingCancel := context.WithTimeout(ctx, 5*time.Second)
	interim := receiveOutageReply(t, pendingCtx, client)
	pendingCancel()
	assertOutagePending(t, request, interim)
	awaitOutageReach(t, ctx, proxy, method, metadata, permanent)
	// The same connection must answer ECHO while the storage request waits.
	echo, err := wire.EncodeEchoRequest(wire.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	echoCtx, echoCancel := context.WithTimeout(ctx, time.Second)
	response := outageExchange(t, echoCtx, client, message(wire.Echo, echo))
	echoCancel()
	if response.Header.Status != smb.StatusSuccess || time.Since(start) >= outage && !permanent {
		t.Fatalf("ECHO did not succeed during the outage: %+v after %s", response.Header, time.Since(start))
	}
	finishCtx, finishCancel := context.WithTimeout(ctx, bound)
	final := receiveOutageReply(t, finishCtx, client)
	finishCancel()
	if final.Header.MessageID != request.Header.MessageID || final.Header.SessionID != session.SessionID || final.Header.Command != command || final.Header.AsyncID != interim.Header.AsyncID || final.Header.Flags&wire.FlagAsync == 0 || final.Header.Credit != 0 {
		t.Fatalf("final identity or credits: %+v after %+v", final.Header, interim.Header)
	}
	if permanent {
		if time.Since(start) < 360*time.Second || final.Header.Status != smb.StatusIODeviceError {
			t.Fatalf("expected actual storage exhaustion after the retry window: %+v after %s", final.Header, time.Since(start))
		}
		if _, err := wire.DecodeErrorResponse(final); err != nil {
			t.Fatal(err)
		}
		if err := proxy.SetFault(s3fault.Fault{}); err != nil {
			t.Fatal(err)
		}
		// Retry the cold read on the same open after S3 returns.
		assertOutageSuccess(t, command, outageExchange(t, ctx, client, message(command, body)), data)
	} else {
		if time.Since(start) < outage {
			t.Fatal("storage operation completed before S3 returned")
		}
		assertOutageSuccess(t, command, final, data)
		if command == wire.Write {
			ledger.Write(path, 0, data)
		}
		ledger.Flush(path)
	}
	closedBody, err := wire.EncodeCloseRequest(wire.CloseRequest{ID: open.ID})
	if err != nil {
		t.Fatal(err)
	}
	if closed := outageExchange(t, ctx, client, message(wire.Close, closedBody)); closed.Header.Status != smb.StatusSuccess {
		t.Fatalf("CLOSE after recovery: %+v", closed.Header)
	}
	func() {
		share, disconnect := f.share()
		defer disconnect()
		share = share.WithContext(ctx)
		const nextPath = "next-backup-band"
		ledger.Attempt(nextPath, 0, data)
		writeFile(t, share, nextPath, data)
		ledger.Write(nextPath, 0, data)
		ledger.Flush(nextPath)
		if err := ledger.CheckAcknowledged(chaosRead(share.ReadFile)); err != nil {
			t.Fatal(err)
		}
		if metadata {
			awaitOutageReceipt(t, ctx, f, baseline, start)
		}
	}()
	t.Logf("operation %d recovered after %s; permanent=%t", command, time.Since(start), permanent)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	d.stop()
	startOutageDaemon(t, f)
	share, disconnect := f.share()
	defer disconnect()
	if err := ledger.CheckFlushed(chaosRead(share.WithContext(ctx).ReadFile)); err != nil {
		t.Fatal("cold verification after recovery:", err)
	}
}

func startOutageDaemon(t *testing.T, f *fixture) *daemon {
	t.Helper()
	d := f.start()
	t.Cleanup(func() {
		d.stop()
		for _, log := range d.logs {
			data, err := os.ReadFile(log.Name())
			if err != nil {
				t.Error(err)
				continue
			}
			if err := chaos.CheckDaemonLog(data); err != nil {
				t.Errorf("daemon generation log %s: %v", log.Name(), err)
			}
		}
	})
	return d
}

func outageClient(t *testing.T, ctx context.Context, f *fixture) (*smbtest.Client, smbtest.Session) {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", f.connectAddr())
	if err != nil {
		t.Fatal(err)
	}
	client, err := smbtest.NewClient(conn)
	if err != nil {
		if closeErr := conn.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	session, err := client.Login(ctx, smbtest.LoginOptions{Share: "TimeMachine", Account: auth.Account{User: "backup", Password: f.password}, Cipher: smb.CipherAES256GCM, Signing: smb.SigningGMAC})
	if err != nil {
		t.Fatal(err)
	}
	return client, session
}

func receiveOutageReply(t *testing.T, ctx context.Context, client *smbtest.Client) wire.Message {
	t.Helper()
	reply, err := client.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(reply.Messages) != 1 {
		t.Fatalf("expected one reply, got %d", len(reply.Messages))
	}
	return reply.Messages[0]
}

func outageExchange(t *testing.T, ctx context.Context, client *smbtest.Client, request wire.Message) wire.Message {
	t.Helper()
	if err := client.Send(ctx, []wire.Message{request}); err != nil {
		t.Fatal(err)
	}
	response := receiveOutageReply(t, ctx, client)
	if response.Header.Status == smb.StatusPending {
		assertOutagePending(t, request, response)
		response = receiveOutageReply(t, ctx, client)
	}
	if response.Header.MessageID != request.Header.MessageID || response.Header.Command != request.Header.Command {
		t.Fatalf("unexpected reply: %+v for %+v", response.Header, request.Header)
	}
	return response
}

func assertOutagePending(t *testing.T, request, response wire.Message) {
	t.Helper()
	if response.Header.Status != smb.StatusPending || response.Header.Flags&wire.FlagAsync == 0 || response.Header.AsyncID == 0 || response.Header.MessageID != request.Header.MessageID || response.Header.SessionID != request.Header.SessionID || response.Header.Command != request.Header.Command || response.Header.Credit != request.Header.Credit {
		t.Fatalf("expected interim pending reply: %+v for %+v", response.Header, request.Header)
	}
	if _, err := wire.DecodeErrorResponse(response); err != nil {
		t.Fatal(err)
	}
}

func assertOutageSuccess(t *testing.T, command wire.Command, response wire.Message, data []byte) {
	t.Helper()
	if response.Header.Status != smb.StatusSuccess {
		t.Fatalf("storage operation failed: %+v", response.Header)
	}
	switch command {
	case wire.Read:
		read, err := wire.DecodeReadResponse(response)
		if err != nil || !bytes.Equal(read.Data, data) {
			t.Fatalf("READ returned different bytes: %v", err)
		}
	case wire.Write:
		write, err := wire.DecodeWriteResponse(response)
		if err != nil || int(write.Count) != len(data) {
			t.Fatalf("WRITE count: %+v %v", write, err)
		}
	case wire.Flush:
		if _, err := wire.DecodeFlushResponse(response); err != nil {
			t.Fatal(err)
		}
	}
}

func awaitOutageReach(t *testing.T, ctx context.Context, proxy *s3fault.Proxy, method string, metadata, permanent bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	chunkSeen := false
	metadataPaths := make(map[string]bool)
	for !chunkSeen || metadata && len(metadataPaths) < 2 {
		select {
		case event := <-proxy.Events():
			kind := "outage"
			if permanent {
				kind = "status"
			}
			if event.Kind != kind || event.Status != http.StatusServiceUnavailable {
				continue
			}
			if strings.Contains(event.Path, "/chunks/") && event.Method == method {
				chunkSeen = true
			}
			if strings.Contains(event.Path, "/meta/") {
				metadataPaths[event.Path] = true
			}
		case <-ctx.Done():
			t.Fatalf("outage did not reach %s chunk or metadata retries: chunk=%t metadata paths=%d: %v", method, chunkSeen, len(metadataPaths), ctx.Err())
		}
	}
}

func outageReceipt(t *testing.T, f *fixture) backup.Receipt {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.root, "state", "backup-receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	var receipt backup.Receipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	return receipt
}

func awaitOutageReceipt(t *testing.T, ctx context.Context, f *fixture, baseline backup.Receipt, start time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		receipt := outageReceipt(t, f)
		if receipt.Key != baseline.Key && receipt.Snapshot.After(start) {
			digest := protectionObjectDigest(t, f, protectionObjectKey(t, f, receipt.Key))
			if hex.EncodeToString(digest[:]) != receipt.SHA256 {
				t.Fatal("recovered metadata backup differs from its receipt")
			}
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("metadata backup did not recover: %v", ctx.Err())
		}
	}
}
