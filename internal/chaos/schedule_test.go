// SPDX-License-Identifier: AGPL-3.0-only

package chaos

import (
	"bufio"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/netfault"
	"github.com/djosh34/s3-smb/internal/s3fault"
)

func proxies(t *testing.T) (*netfault.Proxy, *s3fault.Proxy) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	network, err := netfault.New(t.Context(), strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := network.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	s3, err := s3fault.New(t.Context(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := s3.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return network, s3
}

func status(t *testing.T, url string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, res.Body)
	if err := errors.Join(readErr, res.Body.Close()); err != nil {
		t.Fatal(err)
	}
	return res.StatusCode
}

func TestGenerate(t *testing.T) {
	a, err := Generate(349, 20, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(349, 20, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) || a.String() != b.String() {
		t.Fatal("same seed produced different schedules")
	}
	c, err := Generate(350, 20, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(a, c) {
		t.Fatal("different seeds produced the same schedule")
	}
	last := a[len(a)-1]
	if last.At != 20*time.Second || *last.Net != (netfault.Fault{}) || *last.S3 != (s3fault.Fault{}) || last.S3Outage != 0 {
		t.Fatal("missing restoration step")
	}
	for _, args := range []struct {
		count    int
		interval time.Duration
	}{{0, time.Second}, {-1, time.Second}, {1, 0}, {1, -1}, {2, math.MaxInt64}} {
		if _, err := Generate(1, args.count, args.interval); err == nil {
			t.Fatal("accepted invalid generation arguments")
		}
	}
}

func TestScheduleOrderAndOffsets(t *testing.T) {
	network, s3 := proxies(t)
	s := Schedule{
		{S3: &s3fault.Fault{Status: 503}, Net: &netfault.Fault{Drop: true}},
		{At: 20 * time.Millisecond, S3: &s3fault.Fault{}, Net: &netfault.Fault{}},
		{At: 20 * time.Millisecond, S3: &s3fault.Fault{Status: 429}},
	}
	start := time.Now()
	if err := s.Run(t.Context(), network, s3); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 20*time.Millisecond {
		t.Fatal("step ran before its offset")
	}
	if got := status(t, s3.URL()); got != 429 {
		t.Fatalf("same-offset steps ran out of order: %d", got)
	}
	if got := status(t, "http://"+network.Address()); got != 204 {
		t.Fatalf("network not restored: %d", got)
	}
	if err := (Schedule{{}}).Run(t.Context(), network, s3); err != nil {
		t.Fatal(err)
	}
	if got := status(t, s3.URL()); got != 429 {
		t.Fatal("nil S3 field changed the fault")
	}
}

func TestScheduleCancel(t *testing.T) {
	_, s3 := proxies(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := Schedule{{S3: &s3fault.Fault{Status: 503}}, {At: time.Hour, S3: &s3fault.Fault{}}}
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, nil, s3) }()
	deadline := time.Now().Add(time.Second)
	for status(t, s3.URL()) != 503 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("first step never ran")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("schedule did not stop on cancel")
	}
	if got := status(t, s3.URL()); got != 503 {
		t.Fatal("cancel applied a later step or restored faults")
	}
	if err := (Schedule{{S3: &s3fault.Fault{}}}).Run(ctx, nil, s3); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestScheduleOutage(t *testing.T) {
	_, s3 := proxies(t)
	if err := (Schedule{{S3Outage: time.Hour}}).Run(t.Context(), nil, s3); err != nil {
		t.Fatal(err)
	}
	if got := status(t, s3.URL()); got != 503 {
		t.Fatalf("outage not applied: %d", got)
	}
	s3.RestoreS3()
	if got := status(t, s3.URL()); got != 204 {
		t.Fatalf("outage not restored: %d", got)
	}
}

func TestScheduleCut(t *testing.T) {
	network, _ := proxies(t)
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp", network.Address())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	// A completed request proves the connection was accepted before the cut.
	if _, writeErr := conn.Write([]byte("GET / HTTP/1.0\r\nConnection: keep-alive\r\n\r\n")); writeErr != nil {
		t.Fatal(writeErr)
	}
	if deadlineErr := conn.SetReadDeadline(time.Now().Add(time.Second)); deadlineErr != nil {
		t.Fatal(deadlineErr)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, res.Body)
	if err := errors.Join(readErr, res.Body.Close()); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 1)
	if err := (Schedule{{Cut: true}}).Run(t.Context(), network, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(buffer); err == nil {
		t.Fatal("connection survived cut")
	} else if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Fatalf("cut left a stalled connection: %v", err)
		}
	}
}

func TestScheduleValidation(t *testing.T) {
	network, s3 := proxies(t)
	for _, s := range []Schedule{
		{{At: -1}},
		{{At: 2}, {At: 1}},
		{{S3Outage: -1}},
		{{Net: &netfault.Fault{Delay: -1}}},
		{{S3: &s3fault.Fault{HeaderDelay: -1}}},
		{{S3: &s3fault.Fault{BodyDelay: -1}}},
		{{S3: &s3fault.Fault{CutAfter: -1}}},
		{{S3: &s3fault.Fault{Status: 200}}},
		{{S3: &s3fault.Fault{Status: 600}}},
	} {
		if err := s.Run(t.Context(), network, s3); err == nil {
			t.Fatalf("accepted invalid schedule %s", s)
		}
	}
	for _, s := range []Schedule{{{Net: &netfault.Fault{}}}, {{Cut: true}}, {{S3: &s3fault.Fault{}}}, {{S3Outage: time.Second}}} {
		if err := s.Run(t.Context(), nil, nil); err == nil {
			t.Fatal("accepted missing proxy")
		}
	}
	// Invalid later steps must not apply earlier faults.
	if err := (Schedule{{S3: &s3fault.Fault{Status: 503}}, {At: -1}}).Run(t.Context(), nil, s3); err == nil {
		t.Fatal("accepted unordered schedule")
	}
	if got := status(t, s3.URL()); got != 204 {
		t.Fatal("invalid schedule applied its prefix")
	}
	if err := (Schedule{}).Run(t.Context(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := network.Close(); err != nil {
		t.Fatal(err)
	}
	if err := (Schedule{{Net: &netfault.Fault{}}}).Run(t.Context(), network, nil); !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
}

func TestSchedulePrevalidatesCutThresholds(t *testing.T) {
	for _, test := range []struct {
		name string
		step Step
	}{
		{name: "network negative threshold", step: Step{Net: &netfault.Fault{CutAfter: -1}}},
		{name: "network missing direction", step: Step{Net: &netfault.Fault{CutAfter: 1}}},
		{name: "network invalid direction", step: Step{Net: &netfault.Fault{CutDirection: netfault.ServerToClient + 1}}},
		{name: "S3 negative request threshold", step: Step{S3: &s3fault.Fault{RequestCutAfter: -1}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			network, s3 := proxies(t)
			s := Schedule{{S3: &s3fault.Fault{Status: 503}}, test.step}
			if err := s.Run(t.Context(), network, s3); err == nil {
				t.Fatal("accepted invalid cut fault")
			}
			if got := status(t, s3.URL()); got != 204 {
				t.Fatalf("invalid later fault applied the earlier step: %d", got)
			}
		})
	}
}

func TestScheduleSnapshotAndString(t *testing.T) {
	network, s3 := proxies(t)
	s := Schedule{{At: time.Second, Net: &netfault.Fault{Delay: time.Millisecond}, Cut: true, S3: &s3fault.Fault{Status: 503}, S3Outage: time.Second}, {At: 2 * time.Second}}
	copy, err := s.prepare(network, s3)
	if err != nil {
		t.Fatal(err)
	}
	text := copy.String()
	s[0].Net.Delay = time.Hour
	s[0].S3.Status = 429
	s[0].At = 0
	if copy.String() != text {
		t.Fatal("snapshot shared mutable fault values")
	}
	for _, want := range []string{"at=1s", "Delay:1ms", "cut=true", "Status:503", "outage=1s", "unchanged"} {
		if !strings.Contains(text, want) {
			t.Fatalf("schedule text missing %q: %s", want, text)
		}
	}
}
