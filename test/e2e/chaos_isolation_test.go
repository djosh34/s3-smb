// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/chaos"
	"github.com/djosh34/s3-smb/internal/netfault"
	"github.com/djosh34/s3-smb/internal/smb"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/smbtest"
	"github.com/djosh34/s3-smb/internal/smb/wire"
	smbclient "github.com/hirochachacha/go-smb2"
)

const isolationMaxLatency = 2 * time.Second

type isolationPlan struct {
	activate chaos.Schedule
	cut      chaos.Schedule
	restore  chaos.Schedule
	hold     time.Duration
	probes   int
}

func makeIsolationPlan(seed uint64, kind string, gate bool) isolationPlan {
	random := chaos.Rand(seed, "isolation/"+kind)
	hold := 4*time.Second + time.Duration(random.IntN(1000))*time.Millisecond
	probes := 17
	if gate {
		hold += 26 * time.Second
		probes = 121
	}
	fault := netfault.Fault{Stall: true}
	if kind == "slow" {
		fault = netfault.Fault{Delay: 400*time.Millisecond + time.Duration(random.IntN(200))*time.Millisecond}
	}
	plan := isolationPlan{
		activate: chaos.Schedule{{At: time.Duration(50+random.IntN(100)) * time.Millisecond, Net: &fault}},
		restore:  chaos.Schedule{{Net: &netfault.Fault{}}},
		hold:     hold,
		probes:   probes,
	}
	if kind == "cut" {
		plan.cut = chaos.Schedule{{At: time.Duration(150+random.IntN(100)) * time.Millisecond, Cut: true}}
	}
	return plan
}

func isolationBytes(seed uint64, stream string, size int) []byte {
	random := chaos.Rand(seed, "isolation/bytes/"+stream)
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(random.Uint32())
	}
	return data
}

// The proxy has exactly one SMB workload connection. The other SMB workload
// connects directly. go-smb2 has no public ECHO method, so a direct authenticated
// raw connection checks ECHO alongside the healthy file operations.
func TestChaosConnectionIsolation(t *testing.T) {
	seed := chaos.Seed(t)
	for _, kind := range []string{"stall", "slow", "cut"} {
		for _, operation := range []string{"read", "write"} {
			t.Run(kind+"/"+operation, func(t *testing.T) {
				runIsolation(t, seed, kind, operation)
			})
		}
	}
}

func runIsolation(t *testing.T, seed uint64, kind, operation string) {
	t.Helper()
	plan := makeIsolationPlan(seed, kind, os.Getenv("S3_SMB_CHECK_MODE") == "gate")
	t.Logf("fault hold=%s\nactivate:\n%s\nrestore after healthy probes:\n%s", plan.hold, plan.activate, plan.restore)
	f := newChaosFixture(t, false)
	d := f.start()
	generation := f.generation
	t.Cleanup(func() {
		d.stop()
		for _, log := range d.logs {
			data, err := os.ReadFile(log.Name())
			if err != nil {
				t.Errorf("daemon generation %d log: %v", generation, err)
				continue
			}
			if err := chaos.CheckDaemonLog(data); err != nil {
				t.Errorf("daemon generation %d %s: %v", generation, log.Name(), err)
			}
		}
	})
	proxy, err := netfault.New(t.Context(), f.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := proxy.Close(); err != nil {
			t.Errorf("close isolation proxy: %v", err)
		}
	})
	f.clientAddr = proxy.Address()
	faultShare, closeFault := f.share()
	t.Cleanup(closeFault)
	f.clientAddr = ""
	healthy, closeHealthy := f.share()
	t.Cleanup(closeHealthy)
	echo := newIsolationEcho(t, f)
	ledger := chaos.NewLedger()
	payload := isolationBytes(seed, kind+"/"+operation+"/fault", 4<<20)
	isolationWrite(t, healthy, ledger, "fault.bin", payload, 20*time.Second)
	if operation == "write" {
		payload = isolationBytes(seed, kind+"/write/attempt", len(payload))
	}
	healthyData := isolationBytes(seed, "healthy", 64<<10)
	bound := isolationBaseline(t, healthy, echo, ledger, healthyData)
	t.Logf("healthy latency bound=%s", bound)

	faultCtx, cancelFault := context.WithTimeout(t.Context(), plan.hold+45*time.Second)
	defer cancelFault()
	file, err := faultShare.WithContext(faultCtx).OpenFile("fault.bin", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	fileClosed := false
	t.Cleanup(func() {
		if !fileClosed {
			cancelFault()
			if err := file.Close(); err != nil {
				t.Logf("close canceled fault handle: %v", err)
			}
		}
	})
	if err := plan.activate.Run(t.Context(), proxy, nil); err != nil {
		t.Fatal(err)
	}
	faultStarted := time.Now()
	result, started := startIsolationOperation(file, ledger, payload, operation)
	consumed := false
	t.Cleanup(func() {
		cancelFault()
		if err := proxy.Close(); err != nil {
			t.Errorf("unblock fault client: %v", err)
		}
		if !consumed {
			select {
			case <-result:
			case <-time.After(2 * time.Second):
				t.Error("fault operation did not stop after cancellation and proxy close")
			}
		}
	})
	// Wait for entry into the fault operation, then require it to remain pending
	// before starting healthy probes. Traffic stays faulty until assertions end.
	select {
	case <-started:
	case <-faultCtx.Done():
		t.Fatal(faultCtx.Err())
	}
	select {
	case got := <-result:
		consumed = true
		t.Fatalf("fault client completed before healthy probes: %v", got.err)
	case <-time.After(200 * time.Millisecond):
	}
	if kind == "cut" {
		t.Logf("cut blocked client:\n%s", plan.cut)
		if err := plan.cut.Run(t.Context(), proxy, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Probe throughout the fault window, including its final offset. Even if a
	// probe is late, nothing restores traffic until that probe has passed.
	for i := range plan.probes {
		offset := time.Duration(i) * plan.hold / time.Duration(plan.probes-1)
		timer := time.NewTimer(time.Until(faultStarted.Add(offset)))
		select {
		case <-timer.C:
		case <-t.Context().Done():
			timer.Stop()
			t.Fatal(t.Context().Err())
		}
		timer.Stop()
		data := isolationBytes(seed, fmt.Sprintf("healthy/%s/%s/%d", kind, operation, i), len(healthyData))
		isolationProbe(t, healthy, echo, ledger, data, bound)
	}
	if kind != "cut" {
		select {
		case got := <-result:
			consumed = true
			t.Fatalf("fault client finished before restoration: %v", got.err)
		default:
		}
	}
	if err := plan.restore.Run(t.Context(), proxy, nil); err != nil {
		t.Fatal(err)
	}
	var got isolationResult
	select {
	case got = <-result:
		consumed = true
	case <-faultCtx.Done():
		t.Fatalf("fault operation completion: %v", faultCtx.Err())
	}
	if kind == "cut" {
		if got.err == nil {
			t.Fatal("cut operation reported success")
		}
		t.Logf("cut %s failed visibly: %v", operation, got.err)
	} else if got.err != nil || got.n != len(payload) || operation == "read" && !bytes.Equal(got.data, payload) {
		t.Fatalf("restored %s: bytes=%d/%d error=%v", operation, got.n, len(payload), got.err)
	}
	closeErr := file.Close()
	fileClosed = true
	if closeErr != nil {
		if kind != "cut" {
			t.Fatal(closeErr)
		}
		t.Logf("close cut handle failed visibly: %v", closeErr)
	}
	isolationProbe(t, healthy, echo, ledger, healthyData, bound)
	if err := ledger.CheckAcknowledged(chaosRead(func(name string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(t.Context(), bound)
		defer cancel()
		return healthy.WithContext(ctx).ReadFile(name)
	})); err != nil {
		t.Fatal(err)
	}
}

type isolationResult struct {
	data []byte
	err  error
	n    int
}

func startIsolationOperation(file *smbclient.File, ledger *chaos.Ledger, payload []byte, operation string) (<-chan isolationResult, <-chan struct{}) {
	result := make(chan isolationResult, 1)
	started := make(chan struct{})
	if operation == "write" {
		ledger.Attempt("fault.bin", 0, payload)
	}
	go func() {
		got := isolationResult{}
		if operation == "read" {
			got.data = make([]byte, len(payload))
			close(started)
			got.n, got.err = io.ReadFull(file, got.data)
		} else {
			close(started)
			got.n, got.err = file.WriteAt(payload, 0)
			if got.n > 0 {
				ledger.Write("fault.bin", 0, payload[:got.n])
			}
		}
		result <- got
	}()
	return result, started
}

func isolationWrite(t *testing.T, share *smbclient.Share, ledger *chaos.Ledger, name string, data []byte, bound time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), bound)
	defer cancel()
	started := time.Now()
	file, err := share.WithContext(ctx).Create(name)
	if err != nil {
		t.Fatal(err)
	}
	ledger.Truncate(name, 0)
	ledger.Attempt(name, 0, data)
	n, writeErr := file.Write(data)
	if n > 0 {
		ledger.Write(name, 0, data[:n])
	}
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil || n != len(data) {
		t.Fatalf("healthy write %s: %d/%d %v", name, n, len(data), err)
	}
	if elapsed := time.Since(started); elapsed > bound {
		t.Fatalf("healthy write took %s, limit %s", elapsed, bound)
	}
}

func isolationProbe(t *testing.T, share *smbclient.Share, echo *isolationEcho, ledger *chaos.Ledger, data []byte, bound time.Duration) time.Duration {
	t.Helper()
	started := time.Now()
	isolationWrite(t, share, ledger, "healthy.bin", data, bound)
	maximum := time.Since(started)
	ctx, cancel := context.WithTimeout(t.Context(), bound)
	defer cancel()
	started = time.Now()
	got, err := share.WithContext(ctx).ReadFile("healthy.bin")
	elapsed := time.Since(started)
	if err != nil || !bytes.Equal(got, data) || elapsed > bound {
		t.Fatalf("healthy read: bytes correct=%t elapsed=%s limit=%s error=%v", bytes.Equal(got, data), elapsed, bound, err)
	}
	maximum = max(maximum, elapsed)
	started = time.Now()
	if err := echo.exchange(t.Context(), bound); err != nil {
		t.Fatal(err)
	}
	return max(maximum, time.Since(started))
}

func isolationBaseline(t *testing.T, share *smbclient.Share, echo *isolationEcho, ledger *chaos.Ledger, data []byte) time.Duration {
	t.Helper()
	var baseline time.Duration
	for range 3 {
		baseline = max(baseline, isolationProbe(t, share, echo, ledger, data, isolationMaxLatency))
	}
	return min(isolationMaxLatency, max(500*time.Millisecond, 5*baseline))
}

type isolationEcho struct {
	client  *smbtest.Client
	session smbtest.Session
}

func newIsolationEcho(t *testing.T, f *fixture) *isolationEcho {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", f.addr)
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
			t.Errorf("close healthy ECHO client: %v", err)
		}
	})
	session, err := client.Login(ctx, smbtest.LoginOptions{Share: "TimeMachine", Account: auth.Account{User: "backup", Password: f.password}, Signing: smb.SigningCMAC})
	if err != nil {
		t.Fatal(err)
	}
	return &isolationEcho{client: client, session: session}
}

func (e *isolationEcho) exchange(parent context.Context, bound time.Duration) error {
	ctx, cancel := context.WithTimeout(parent, bound)
	defer cancel()
	body, err := wire.EncodeEchoRequest(wire.EmptyRequest{})
	if err != nil {
		return err
	}
	id := e.session.NextMessageID
	e.session.NextMessageID++
	request := wire.Message{Header: wire.Header{Command: wire.Echo, MessageID: id, SessionID: e.session.SessionID, CreditCharge: 1, Credit: 1}, Body: body}
	started := time.Now()
	if err := e.client.Send(ctx, []wire.Message{request}); err != nil {
		return fmt.Errorf("healthy ECHO send: %w", err)
	}
	reply, err := e.client.Receive(ctx)
	if err != nil {
		return fmt.Errorf("healthy ECHO receive: %w", err)
	}
	if len(reply.Messages) != 1 {
		return fmt.Errorf("healthy ECHO returned %d messages", len(reply.Messages))
	}
	message := reply.Messages[0]
	if message.Header.Command != wire.Echo || message.Header.MessageID != id || message.Header.SessionID != e.session.SessionID || message.Header.Status != smb.StatusSuccess {
		return fmt.Errorf("healthy ECHO identity or status mismatch: %+v", message.Header)
	}
	if _, err := wire.DecodeEchoResponse(message); err != nil {
		return err
	}
	if elapsed := time.Since(started); elapsed > bound {
		return fmt.Errorf("healthy ECHO took %s, limit %s", elapsed, bound)
	}
	return nil
}

func TestIsolationPlan(t *testing.T) {
	for _, kind := range []string{"stall", "slow", "cut"} {
		first := makeIsolationPlan(357, kind, false)
		if !reflect.DeepEqual(first, makeIsolationPlan(357, kind, false)) {
			t.Fatal("seed did not replay the plan")
		}
		if reflect.DeepEqual(first, makeIsolationPlan(358, kind, false)) {
			t.Fatal("different seeds made the same plan")
		}
		gate := makeIsolationPlan(357, kind, true)
		if first.probes != 17 || gate.probes != 121 {
			t.Fatal("missing probes across the full fault window")
		}
		if first.hold < 4*time.Second || first.hold >= 5*time.Second || gate.hold < 30*time.Second || gate.hold >= 31*time.Second {
			t.Fatalf("unexpected fault lengths: PR=%s gate=%s", first.hold, gate.hold)
		}
		if first.activate[0].At < 50*time.Millisecond || first.activate[0].At >= 150*time.Millisecond {
			t.Fatal("activation offset is outside the seeded range")
		}
		if kind == "cut" && (len(first.cut) != 1 || !first.cut[0].Cut || first.cut[0].At < 150*time.Millisecond || first.cut[0].At >= 250*time.Millisecond) {
			t.Fatal("missing seeded cut")
		}
		if !reflect.DeepEqual(*first.restore[0].Net, netfault.Fault{}) {
			t.Fatal("restoration retained faults")
		}
		if kind == "slow" && first.activate[0].Net.Delay <= 0 || kind != "slow" && !first.activate[0].Net.Stall {
			t.Fatal("missing isolation fault")
		}
	}
	first := isolationBytes(357, "read", 1024)
	if !bytes.Equal(first, isolationBytes(357, "read", 1024)) || bytes.Equal(first, isolationBytes(357, "write", 1024)) {
		t.Fatal("payload streams do not replay independently")
	}
}
