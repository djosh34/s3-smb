// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testProxy(t *testing.T, upstream http.Handler) (*proxy, *httptest.Server, *httptest.Server, *bytes.Buffer) {
	t.Helper()
	backend := httptest.NewServer(upstream)
	t.Cleanup(backend.Close)
	u, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	var evidence bytes.Buffer
	p := newProxy(u, &evidence)
	data, control := httptest.NewServer(p), httptest.NewServer(p.control())
	t.Cleanup(func() {
		p.release()
		data.Close()
		control.Close()
		p.forward.Transport.(*http.Transport).CloseIdleConnections()
	})
	return p, data, control, &evidence
}

func stateRequest(t *testing.T, control *httptest.Server, method, path string) proxyState {
	t.Helper()
	req, err := http.NewRequest(method, control.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := control.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("control %s %s status %d", method, path, res.StatusCode)
	}
	var state proxyState
	if err := json.NewDecoder(res.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if !state.EvidenceOK {
		t.Fatal("evidence unavailable")
	}
	return state
}

func waitState(t *testing.T, control *httptest.Server, predicate func(proxyState) bool) proxyState {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state := stateRequest(t, control, http.MethodGet, "/state")
		if predicate(state) {
			return state
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("proxy state deadline")
	return proxyState{}
}

func request(t *testing.T, client *http.Client, method, target, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(data)
}

func events(t *testing.T, p *proxy, evidence *bytes.Buffer) []event {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	decoder := json.NewDecoder(bytes.NewReader(evidence.Bytes()))
	var result []event
	for {
		var e event
		err := decoder.Decode(&e)
		if err == io.EOF {
			return result
		}
		if err != nil {
			t.Fatal(err)
		}
		if e.Sequence != uint64(len(result)+1) || e.Time.IsZero() {
			t.Fatalf("invalid ordered timestamped evidence: %+v", e)
		}
		result = append(result, e)
	}
}

func TestForwardingAndCompletedReadEvidence(t *testing.T) {
	observed := make(chan [3]string, 1)
	p, data, control, evidence := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- [3]string{r.Host, r.Header.Get("Authorization"), r.URL.RawQuery}
		w.WriteHeader(http.StatusPartialContent)
		io.WriteString(w, "remote-bytes")
	}))
	req, err := http.NewRequest(http.MethodGet, data.URL+"/bucket/volume/chunks/0/1?signature=private-query-marker", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1:19001"
	req.Header.Set("Authorization", "private-header-marker")
	res, err := data.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || string(body) != "remote-bytes" || res.StatusCode != 206 {
		t.Fatalf("forwarded response: %d %q %v", res.StatusCode, body, err)
	}
	state := waitState(t, control, func(s proxyState) bool { return s.ChunkGetSuccess == 1 })
	got := <-observed
	if state.ChunkGetSuccessBytes != int64(len(body)) || got[0] != req.Host || got[1] != "private-header-marker" || got[2] != req.URL.RawQuery {
		t.Fatalf("forwarding/counters changed: %+v host=%s", state, got[0])
	}
	if len(events(t, p, evidence)) != 3 {
		t.Fatal("expected request/upstream/completion events")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if bytes.Contains(evidence.Bytes(), []byte("private-")) {
		t.Fatal("headers/query leaked to evidence")
	}
}

func TestHoldSuccessfulPutResponseReleaseAndExactIdentity(t *testing.T) {
	var committed atomic.Bool
	p, data, control, evidence := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != "uploaded-before-hold" {
			t.Error("upstream did not receive uploaded body")
		}
		committed.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	stateRequest(t, control, http.MethodPost, "/hold")
	result := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPut, data.URL+"/bucket/volume/chunks/0/held", strings.NewReader("uploaded-before-hold"))
		if err == nil {
			var res *http.Response
			res, err = data.Client().Do(req)
			if err == nil {
				res.Body.Close()
				if res.StatusCode != 200 {
					err = errors.New("wrong released status")
				}
			}
		}
		result <- err
	}()
	state := waitState(t, control, func(s proxyState) bool { return s.PendingCount == 1 })
	if !committed.Load() || !state.Hold.Pending || state.Hold.UpstreamStatus != 200 || state.Hold.Path != "/bucket/volume/chunks/0/held" || state.Hold.Key != "volume/chunks/0/held" || state.Hold.RequestID == 0 || state.Armed {
		t.Fatalf("not a committed PUT with one exact pending response: %+v", state)
	}
	select {
	case err := <-result:
		t.Fatalf("client completed during hold: %v", err)
	default:
	}
	if status, _ := request(t, control.Client(), http.MethodPost, control.URL+"/hold", ""); status != 409 {
		t.Fatal("double arm must conflict")
	}
	released := stateRequest(t, control, http.MethodPost, "/release")
	if released.PendingCount != 0 || released.Hold.Pending || released.Hold.Outcome != "released" || released.Hold.RequestID != state.Hold.RequestID {
		t.Fatalf("bad release: %+v", released)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("release did not forward response")
	}
	var held, release *event
	for _, e := range events(t, p, evidence) {
		e := e
		if e.Event == "chunk_put_response_held" {
			held = &e
		}
		if e.Event == "chunk_put_response_released" {
			release = &e
		}
	}
	if held == nil || release == nil || held.RequestID != release.RequestID || release.Time.Before(held.Time) {
		t.Fatal("missing correlated hold/release timing")
	}
}

func TestDisconnectClearsPendingAndRetainsEvidence(t *testing.T) {
	p, data, control, evidence := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	stateRequest(t, control, http.MethodPost, "/hold")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, data.URL+"/bucket/v/chunks/1", strings.NewReader("data"))
	done := make(chan error, 1)
	go func() {
		res, err := data.Client().Do(req)
		if res != nil {
			res.Body.Close()
		}
		done <- err
	}()
	waitState(t, control, func(s proxyState) bool { return s.PendingCount == 1 })
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled request succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled client stuck")
	}
	state := waitState(t, control, func(s proxyState) bool { return s.Hold != nil && !s.Hold.Pending })
	if state.PendingCount != 0 || state.Hold.Outcome != "client_disconnected" || state.Hold.EndedAt == nil {
		t.Fatalf("stale pending evidence: %+v", state)
	}
	found := false
	for _, e := range events(t, p, evidence) {
		if e.Event == "chunk_put_client_disconnected" {
			found = true
		}
	}
	if !found {
		t.Fatal("no disconnect event")
	}
}

func TestOnlySuccessfulChunkPutConsumesArm(t *testing.T) {
	_, data, control, _ := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "fail") {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
		io.WriteString(w, "metadata")
	}))
	stateRequest(t, control, http.MethodPost, "/hold")
	for _, item := range []struct{ method, path string }{
		{http.MethodPut, "/bucket/v/chunks/fail"},
		{http.MethodGet, "/bucket/v/chunks/fail"},
		{http.MethodPut, "/bucket/v/meta/dump"},
		{http.MethodGet, "/bucket/v/meta/dump"},
		{http.MethodHead, "/bucket/v/chunks/1"},
	} {
		request(t, data.Client(), item.method, data.URL+item.path, "")
	}
	state := stateRequest(t, control, http.MethodGet, "/state")
	if !state.Armed || state.PendingCount != 0 || state.ChunkGetSuccess != 0 || state.ChunkGetSuccessBytes != 0 {
		t.Fatalf("wrong method/path/status consumed hold or counted GET: %+v", state)
	}
	state = stateRequest(t, control, http.MethodPost, "/release")
	if state.Armed {
		t.Fatal("release did not disarm unused hold")
	}
}

func TestMetadataEventsAndPartialResponseNotSuccessful(t *testing.T) {
	p, data, control, evidence := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/chunks/") {
			w.Header().Set("Content-Length", "100")
		}
		io.WriteString(w, "short")
	}))
	request(t, data.Client(), http.MethodPut, data.URL+"/bucket/v/meta/dump-1", "native-export")
	res, err := data.Client().Get(data.URL + "/bucket/v/chunks/partial")
	if err == nil {
		_, err = io.ReadAll(res.Body)
		res.Body.Close()
	}
	if err == nil {
		t.Fatal("incomplete upstream body must fail client")
	}
	state := stateRequest(t, control, http.MethodGet, "/state")
	if state.ChunkGetSuccess != 0 || state.ChunkGetSuccessBytes != 0 {
		t.Fatal("partial read counted successful")
	}
	found := false
	for _, e := range events(t, p, evidence) {
		if e.Event == "metadata_upstream_success" && e.Method == "PUT" && e.Key == "v/meta/dump-1" && e.UpstreamStatus == 200 {
			found = true
		}
	}
	if !found {
		t.Fatal("missing metadata observation")
	}
}

type failingEvidence struct{ failSync bool }

func (w failingEvidence) Write(b []byte) (int, error) {
	if w.failSync {
		return len(b), nil
	}
	return 0, errors.New("evidence write failure")
}
func (w failingEvidence) Sync() error { return errors.New("evidence sync failure") }

func TestEvidenceFailuresAreTerminal(t *testing.T) {
	for _, syncFailure := range []bool{false, true} {
		upstream, _ := url.Parse("http://127.0.0.1:1")
		p := newProxy(upstream, failingEvidence{failSync: syncFailure})
		w := httptest.NewRecorder()
		p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/bucket/v/chunks/1", nil))
		if w.Code != 503 {
			t.Fatal("evidence failure allowed forwarding")
		}
		select {
		case <-p.failed:
		default:
			t.Fatal("no terminal failure signal")
		}
		state := httptest.NewRecorder()
		p.control().ServeHTTP(state, httptest.NewRequest(http.MethodGet, "/state", nil))
		if state.Code != 503 || !strings.Contains(state.Body.String(), `"evidence_ok":false`) {
			t.Fatal("evidence health falsely good")
		}
	}
}

func TestLoopbackOnlyConfiguration(t *testing.T) {
	for _, good := range []string{"http://127.0.0.1:19000", "http://[::1]:19000/"} {
		if _, err := loopbackURL(good); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []string{"http://localhost:19000", "http://0.0.0.0:19000", "http://192.0.2.1:19000", "http://127.0.0.1", "https://127.0.0.1:19000", "http://a:b@127.0.0.1:19000", "http://127.0.0.1:19000/path", "http://127.0.0.1:19000?q=1"} {
		if _, err := loopbackURL(bad); err == nil {
			t.Fatalf("accepted non-task-private origin %s", bad)
		}
	}
}
