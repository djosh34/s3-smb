// SPDX-License-Identifier: AGPL-3.0-only

package s3fault_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/djosh34/s3-smb/internal/s3fault"
)

func TestCutUploads(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		for _, cut := range []int64{0, 100, 8192, 65536, 65537} {
			t.Run(boolName(chunked)+"/"+strconv.FormatInt(cut, 10), func(t *testing.T) {
				testCutUpload(t, chunked, cut)
			})
		}
	}
}

func testCutUpload(t *testing.T, chunked bool, cut int64) {
	t.Helper()
	data := bytes.Repeat([]byte("abcdefgh"), 8192)
	received := make(chan []byte, 1)
	proxy := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		received <- got
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	setFault(t, proxy, s3fault.Fault{Method: http.MethodPut, PathContains: "/chunks/", CutRequest: true, RequestCutAfter: cut})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, proxy.URL()+"/bucket/chunks/key", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if chunked {
		req.ContentLength = -1
	}
	res := send(req)
	if cut < int64(len(data)) {
		// A reset racing the HTTP client's writer can also surface as a closed
		// connection error. The paused raw TCP test checks the reset itself.
		if res.err == nil || res.status != 0 {
			t.Fatalf("cut upload: status=%d error=%v, want transport failure", res.status, res.err)
		}
		assertEvent(t, proxy, "request-cut", http.MethodPut, 0)
	} else {
		if res.err != nil || res.status != http.StatusNoContent {
			t.Fatalf("uncut upload: status=%d error=%v", res.status, res.err)
		}
		assertNoEvent(t, proxy)
	}
	// A small fixed-length cut need not flush all consumed bytes upstream.
	// Zero-byte cuts may or may not send headers, even with chunked framing.
	if cut == 0 || !chunked && cut < 4096 {
		select {
		case got := <-received:
			if int64(len(got)) > cut || !bytes.Equal(got, data[:len(got)]) {
				t.Fatalf("small cut forwarded %d bytes, want a prefix of at most %d", len(got), cut)
			}
		case <-time.After(50 * time.Millisecond):
		}
		return
	}
	select {
	case got := <-received:
		if !bytes.Equal(got, data[:min(cut, int64(len(data)))]) {
			t.Fatalf("forwarded %d bytes, want %d", len(got), min(cut, int64(len(data))))
		}
	case <-time.After(time.Second):
		t.Fatal("upstream did not observe upload")
	}
}

func boolName(chunked bool) string {
	if chunked {
		return "chunked"
	}
	return "fixed"
}

func assertEvent(t *testing.T, proxy *s3fault.Proxy, kind, method string, status int) {
	t.Helper()
	select {
	case event := <-proxy.Events():
		if event.Kind != kind || event.Method != method || event.Path != "/bucket/chunks/key" || event.Status != status {
			t.Fatalf("event = %+v, want %s %s status %d", event, method, kind, status)
		}
	case <-time.After(time.Second):
		t.Fatalf("missing %s event", kind)
	}
}

func assertNoEvent(t *testing.T, proxy *s3fault.Proxy) {
	t.Helper()
	select {
	case event := <-proxy.Events():
		t.Fatalf("unexpected event %+v", event)
	default:
	}
}

func TestFaultEvents(t *testing.T) {
	proxy := newProxy(t, http.HandlerFunc(payload))
	for _, test := range []struct {
		kind   string
		fault  s3fault.Fault
		status int
	}{
		{"status", s3fault.Fault{Status: 500}, 500},
		{"throttle", s3fault.Fault{Status: 503, Code: "SlowDown"}, 503},
		{"header-delay", s3fault.Fault{HeaderDelay: time.Millisecond}, 200},
		{"body-delay", s3fault.Fault{BodyDelay: time.Millisecond}, 200},
		{"response-cut", s3fault.Fault{CutBody: true, CutAfter: 4}, 200},
	} {
		setFault(t, proxy, test.fault)
		res := fetch(t.Context(), proxy, http.MethodGet, "/bucket/chunks/key?secret=not-recorded")
		if res.err != nil && test.kind != "response-cut" {
			t.Fatal(res.err)
		}
		assertEvent(t, proxy, test.kind, http.MethodGet, test.status)
		assertNoEvent(t, proxy)
	}
	setFault(t, proxy, s3fault.Fault{CutBody: true, CutAfter: 10})
	request(t, proxy, http.MethodGet, "/bucket/chunks/key")
	assertNoEvent(t, proxy)
	setFault(t, proxy, s3fault.Fault{Method: http.MethodPut, CutRequest: true, RequestCutAfter: 0})
	request(t, proxy, http.MethodGet, "/bucket/chunks/key")
	assertNoEvent(t, proxy)
	setFault(t, proxy, s3fault.Fault{})
	proxy.FailS3For(time.Minute)
	request(t, proxy, http.MethodGet, "/bucket/chunks/key")
	assertEvent(t, proxy, "outage", http.MethodGet, 503)
	proxy.RestoreS3()
	proxy.SetMetadataFailure(true)
	request(t, proxy, http.MethodPut, "/bucket/meta/key")
	select {
	case event := <-proxy.Events():
		if event.Kind != "metadata-failure" || event.Path != "/bucket/meta/key" || event.Status != 503 {
			t.Fatalf("metadata event = %+v", event)
		}
	default:
		t.Fatal("missing metadata event")
	}
	proxy.SetMetadataFailure(false)
	held, err := proxy.HoldNextChunkResponse()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan response, 1)
	go func() { done <- fetch(t.Context(), proxy, http.MethodPut, "/bucket/chunks/key") }()
	select {
	case <-held:
	case <-time.After(time.Second):
		t.Fatal("missing hold")
	}
	assertEvent(t, proxy, "hold", http.MethodPut, 200)
	proxy.Release()
	if res := <-done; res.err != nil {
		t.Fatal(res.err)
	}
}

func TestDelayedStatusEvents(t *testing.T) {
	proxy := newProxy(t, http.HandlerFunc(payload))
	for _, code := range []string{"ServiceUnavailable", "SlowDown"} {
		setFault(t, proxy, s3fault.Fault{Status: 503, Code: code, HeaderDelay: time.Millisecond})
		res := request(t, proxy, http.MethodPut, "/bucket/chunks/key")
		if res.status != 503 {
			t.Fatalf("delayed error status=%d, want 503", res.status)
		}
		assertEvent(t, proxy, "header-delay", http.MethodPut, 0)
		kind := "status"
		if code == "SlowDown" {
			kind = "throttle"
		}
		assertEvent(t, proxy, kind, http.MethodPut, 503)
		assertNoEvent(t, proxy)
	}
}

func TestEventOverflow(t *testing.T) {
	proxy := newProxy(t, http.HandlerFunc(payload))
	setFault(t, proxy, s3fault.Fault{Status: 503})
	const count = 300
	for range count {
		request(t, proxy, http.MethodGet, "/bucket/chunks/key")
	}
	var received uint64
	for {
		select {
		case <-proxy.Events():
			received++
		default:
			if received == 0 || received >= count || received+proxy.DroppedEvents() != count {
				t.Fatalf("received=%d dropped=%d, want %d total with overflow", received, proxy.DroppedEvents(), count)
			}
			return
		}
	}
}

func TestNegativeRequestCut(t *testing.T) {
	proxy := newProxy(t, http.HandlerFunc(payload))
	setFault(t, proxy, s3fault.Fault{Status: 503})
	if err := proxy.SetFault(s3fault.Fault{RequestCutAfter: -1}); err == nil {
		t.Fatal("accepted a negative request cut")
	}
	if res := request(t, proxy, http.MethodGet, "/bucket/chunks/key"); res.status != 503 {
		t.Fatal("invalid fault replaced the current fault")
	}
}

func TestCutPausedUploads(t *testing.T) {
	for _, chunked := range []bool{false, true} {
		t.Run(boolName(chunked), func(t *testing.T) {
			proxy := newProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			setFault(t, proxy, s3fault.Fault{CutRequest: true, RequestCutAfter: 4})
			var dialer net.Dialer
			conn, err := dialer.DialContext(t.Context(), "tcp", strings.TrimPrefix(proxy.URL(), "http://"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := conn.Close(); err != nil {
					t.Error(err)
				}
			}()
			if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			framing := "Content-Length: 64\r\n\r\nabcde"
			if chunked {
				framing = "Transfer-Encoding: chunked\r\n\r\n5\r\nabcde\r\n"
			}
			request := fmt.Sprintf("PUT /bucket/chunks/key HTTP/1.1\r\nHost: paused.test\r\n%s", framing)
			if n, err := io.WriteString(conn, request); err != nil || n != len(request) {
				t.Fatalf("send paused upload: %d/%d %v", n, len(request), err)
			}
			var reply [1]byte
			if n, err := conn.Read(reply[:]); n != 0 || !errors.Is(err, syscall.ECONNRESET) {
				t.Fatalf("paused upload: read %d bytes, error=%v, want reset before remaining body", n, err)
			}
			assertEvent(t, proxy, "request-cut", http.MethodPut, 0)
			assertNoEvent(t, proxy)
		})
	}
}
