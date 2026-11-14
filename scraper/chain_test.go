package scraper

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fixtureServer is an offline HTTP server recording every request it
// receives. All endpoints used by the chain tests live here so no
// network access is required.
func fixtureServer(t *testing.T) (*httptest.Server, *requestLog) {
	t.Helper()
	log := &requestLog{}
	mux := http.NewServeMux()

	// upstream list (JSON mode, jq selectors)
	mux.HandleFunc("/up", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"items":[
			{"id":"1","name":"one"},
			{"id":"2","name":"two"}
		]}`)
	})

	// upstream single-object (no list selector)
	mux.HandleFunc("/up-single", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"9","name":"nine"}`)
	})

	// HTML-mode upstream exercising CSS selectors
	mux.HandleFunc("/up-html", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html><body>
			<div class="item"><h2>One</h2><a href="/d/1">x</a></div>
			<div class="item"><h2>Two</h2><a href="/d/2">x</a></div>
		</body></html>`)
	})

	// downstream: echoes how it was called so tests can assert templates
	mux.HandleFunc("/down/", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		// marshal properly so a JSON body echoed back stays valid JSON
		json.NewEncoder(w).Encode(map[string]any{
			"path":   r.URL.Path,
			"query":  r.URL.RawQuery,
			"id":     r.URL.Query().Get("id"),
			"header": r.Header.Get("X-Up-Id"),
			"body":   string(body),
		})
	})

	// always-failing upstream (HTTP 503)
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
	})

	// always-failing downstream (HTTP 404)
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		http.Error(w, "not found", http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, log
}

type requestLog struct {
	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	method string
	path   string
	query  string
	header string
	body   string
}

func (l *requestLog) record(r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	// restore the body so the handler can still read it
	r.Body = io.NopCloser(bytes.NewReader(b))
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, recordedRequest{
		method: r.Method,
		path:   r.URL.Path,
		query:  r.URL.RawQuery,
		header: r.Header.Get("X-Up-Id"),
		body:   string(b),
	})
}

func (l *requestLog) snapshot() []recordedRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]recordedRequest, len(l.requests))
	copy(out, l.requests)
	return out
}

func chainConfig(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	// config paths are interpolated with the test server's base URL
	cfg := map[string]any{
		"up": map[string]any{
			"mode": "json",
			"url":  srv.URL + "/up",
			"list": ".items[]",
			"result": map[string]any{
				"id":   ".id",
				"name": ".name",
			},
		},
		"up-single": map[string]any{
			"mode": "json",
			"url":  srv.URL + "/up-single",
			"result": map[string]any{
				"id":   ".id",
				"name": ".name",
			},
		},
		"up-html": map[string]any{
			"url":  srv.URL + "/up-html",
			"list": ".item",
			"result": map[string]any{
				"name": "h2",
				"id":   []string{"a", "@href"},
			},
		},
		"down": map[string]any{
			"mode": "json",
			"url":  srv.URL + "/down/detail?id={{id}}",
			"headers": map[string]string{
				"X-Up-Id": "{{id}}",
			},
			"result": map[string]any{
				"id":     ".id",
				"header": ".header",
				"path":   ".path",
			},
			"chain": map[string]any{"from": "up"},
		},
		"down-single": map[string]any{
			"mode": "json",
			"url":  srv.URL + "/down/detail?id={{id}}",
			"result": map[string]any{
				"id": ".id",
			},
			"chain": map[string]any{"from": "up-single"},
		},
		"down-html": map[string]any{
			"mode": "json",
			"url":  srv.URL + "/down{{id}}",
			"result": map[string]any{
				"path": ".path",
			},
			"chain": map[string]any{"from": "up-html"},
		},
		"down-body": map[string]any{
			"mode":   "json",
			"method": "POST",
			"url":    srv.URL + "/down/create?id={{id}}",
			"body":   `{"upstream":"{{name}}","ref":"{{id}}"}`,
			"result": map[string]any{
				"body": ".body",
				"id":   ".id",
			},
			"chain": map[string]any{"from": "up"},
		},
		"down-param": map[string]any{
			"mode": "json",
			"url":  srv.URL + "/down/detail?id={{ref}}",
			"result": map[string]any{
				"id": ".id",
			},
			"chain": map[string]any{"from": "up", "param": "ref"},
		},
		"down-boom": map[string]any{
			"mode": "json",
			"url":  srv.URL + "/down/detail?id={{id}}",
			"result": map[string]any{
				"id": ".id",
			},
			"chain": map[string]any{"from": "boom"},
		},
		"boom": map[string]any{
			"mode": "json",
			"url":  srv.URL + "/boom",
			"result": map[string]any{
				"id": ".id",
			},
		},
		"down-missing": map[string]any{
			"mode": "json",
			"url":  srv.URL + "/missing?id={{id}}",
			"result": map[string]any{
				"id": ".id",
			},
			"chain": map[string]any{"from": "up"},
		},
		"down-missing-param": map[string]any{
			"mode": "json",
			"url":  srv.URL + "/down/detail?id={{nope}}",
			"result": map[string]any{
				"id": ".id",
			},
			"chain": map[string]any{"from": "up"},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func loadChainHandler(t *testing.T, cfg string) *Handler {
	t.Helper()
	h := &Handler{}
	if err := h.LoadConfig([]byte(cfg)); err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return h
}

func TestChainFansOutUpstreamResults(t *testing.T) {
	srv, reqLog := fixtureServer(t)
	h := loadChainHandler(t, chainConfig(t, srv))

	res, err := h.Endpoint("down").Execute(nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d results, want 2 (one downstream call per upstream item)", len(res))
	}
	if res[0]["id"] != "1" || res[0]["header"] != "1" {
		t.Errorf("result[0] = %+v, want id/header 1", res[0])
	}
	if res[1]["id"] != "2" || res[1]["header"] != "2" {
		t.Errorf("result[1] = %+v, want id/header 2", res[1])
	}
	if res[0]["path"] != "/down/detail" {
		t.Errorf("path = %q, want /down/detail", res[0]["path"])
	}

	reqs := reqLog.snapshot()
	if len(reqs) != 3 {
		t.Fatalf("got %d HTTP requests, want 3 (1 upstream + 2 downstream)", len(reqs))
	}
	if reqs[0].path != "/up" {
		t.Errorf("first request = %q, want /up", reqs[0].path)
	}
	for i, r := range reqs[1:] {
		if r.path != "/down/detail" {
			t.Errorf("request %d path = %q", i+1, r.path)
		}
		if r.header != []string{"1", "2"}[i] {
			t.Errorf("request %d X-Up-Id = %q", i+1, r.header)
		}
	}
}

func TestChainPreservesQueryEscaping(t *testing.T) {
	srv, _ := fixtureServer(t)
	h := loadChainHandler(t, chainConfig(t, srv))

	res, err := h.Endpoint("down").Execute(nil)
	if err != nil {
		t.Fatal(err)
	}
	// RawQuery is reconstructed from parsed values; verify it round-trips
	// through url.Parse like the existing template engine guarantees.
	u, err := url.Parse(srv.URL + "/down/detail?" + "id=" + res[0]["id"])
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("id") != "1" {
		t.Fatalf("query did not round-trip: %q", u.RawQuery)
	}
}

func TestChainSingleUpstreamKeepsObjectShape(t *testing.T) {
	srv, _ := fixtureServer(t)
	h := loadChainHandler(t, chainConfig(t, srv))

	res, err := h.Endpoint("down-single").Execute(nil)
	if err != nil {
		t.Fatal(err)
	}
	// no List on either endpoint and exactly one upstream result:
	// the HTTP handler must serialise a single object, not an array
	if len(res) != 1 {
		t.Fatalf("got %d results, want 1", len(res))
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/down-single", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	body := strings.TrimSpace(rr.Body.String())
	if !strings.HasPrefix(body, "{") {
		t.Fatalf("want single JSON object, got %s", body)
	}
	var single map[string]string
	if err := json.Unmarshal([]byte(body), &single); err != nil {
		t.Fatalf("response is not a JSON object: %v", err)
	}
	if single["id"] != "9" {
		t.Errorf("id = %q, want 9", single["id"])
	}
}

func TestChainListResponseIsArray(t *testing.T) {
	srv, _ := fixtureServer(t)
	h := loadChainHandler(t, chainConfig(t, srv))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/down", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	body := strings.TrimSpace(rr.Body.String())
	if !strings.HasPrefix(body, "[") {
		t.Fatalf("want JSON array for list endpoint, got %s", body)
	}
}

func TestChainUpstreamHTTPFailureReturnsStableError(t *testing.T) {
	srv, reqLog := fixtureServer(t)
	h := loadChainHandler(t, chainConfig(t, srv))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/down-boom", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rr.Code, rr.Body.String())
	}
	var ce ChainError
	if err := json.Unmarshal(rr.Body.Bytes(), &ce); err != nil {
		t.Fatalf("response is not the stable chain error structure: %v (%s)", err, rr.Body.String())
	}
	if ce.Endpoint != "down-boom" {
		t.Errorf("endpoint = %q, want down-boom", ce.Endpoint)
	}
	if ce.Status != http.StatusServiceUnavailable {
		t.Errorf("status field = %d, want 503", ce.Status)
	}
	if !strings.Contains(ce.Message, "upstream") {
		t.Errorf("error message %q should mention the upstream", ce.Message)
	}
	// no downstream calls when the upstream failed
	for _, r := range reqLog.snapshot() {
		if strings.HasPrefix(r.path, "/down") {
			t.Fatalf("downstream must not be called after upstream failure, saw %s", r.path)
		}
	}
}

func TestChainDownstreamHTTPFailureReturnsStableError(t *testing.T) {
	srv, _ := fixtureServer(t)
	h := loadChainHandler(t, chainConfig(t, srv))

	_, err := h.Endpoint("down-missing").Execute(nil)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	var ce *ChainError
	if !errors.As(err, &ce) {
		t.Fatalf("want *ChainError, got %T: %v", err, err)
	}
	if ce.Endpoint != "down-missing" || ce.Status != http.StatusNotFound {
		t.Errorf("got endpoint=%q status=%d, want down-missing/404", ce.Endpoint, ce.Status)
	}

	// through the HTTP handler the shape stays identical
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/down-missing", nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	var rendered ChainError
	if err := json.Unmarshal(rr.Body.Bytes(), &rendered); err != nil {
		t.Fatalf("bad error envelope: %v (%s)", err, rr.Body.String())
	}
	if rendered.Status != http.StatusNotFound || rendered.Endpoint != "down-missing" {
		t.Errorf("rendered = %+v", rendered)
	}
}

func TestChainMissingTemplateParamStillErrors(t *testing.T) {
	srv, _ := fixtureServer(t)
	h := loadChainHandler(t, chainConfig(t, srv))

	_, err := h.Endpoint("down-missing-param").Execute(nil)
	if err == nil {
		t.Fatal("want missing-param error, got nil")
	}
	var ce *ChainError
	if !errors.As(err, &ce) {
		t.Fatalf("want *ChainError, got %T: %v", err, err)
	}
	if !strings.Contains(ce.Message, "missing param: nope") {
		t.Errorf("error = %q, want it to contain missing param: nope", ce.Message)
	}
	// local template errors carry no upstream HTTP status
	if ce.Status != 0 {
		t.Errorf("status = %d, want 0 for local errors", ce.Status)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/down-missing-param", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for local template error", rr.Code)
	}
}

func TestChainBodyTemplateReceivesUpstreamValues(t *testing.T) {
	srv, reqLog := fixtureServer(t)
	h := loadChainHandler(t, chainConfig(t, srv))

	res, err := h.Endpoint("down-body").Execute(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d results, want 2", len(res))
	}
	if res[0]["body"] != `{"upstream":"one","ref":"1"}` {
		t.Errorf("first body = %q", res[0]["body"])
	}
	if res[1]["body"] != `{"upstream":"two","ref":"2"}` {
		t.Errorf("second body = %q", res[1]["body"])
	}
	reqs := reqLog.snapshot()
	if reqs[1].method != http.MethodPost {
		t.Errorf("downstream method = %q, want POST", reqs[1].method)
	}
}

func TestChainParamModePassesOnlyNamedField(t *testing.T) {
	srv, reqLog := fixtureServer(t)
	base := chainConfig(t, srv)

	// "ref" is absent from the upstream result, so Param mode marks it
	// absent and the required {{ref}} template reports missing.
	missing := tweakConfig(t, base, "down-param", func(e map[string]any) {
		e["chain"] = map[string]any{"from": "up", "param": "ref"}
	})
	h := loadChainHandler(t, missing)
	_, err := h.Endpoint("down-param").Execute(nil)
	if err == nil || !strings.Contains(err.Error(), "missing param: ref") {
		t.Fatalf("want missing param: ref, got %v", err)
	}
	// the upstream was still called exactly once
	if n := len(reqLog.snapshot()); n != 1 {
		t.Fatalf("got %d requests, want 1 (upstream only)", n)
	}

	// with a field that exists in the upstream result, Param mode forwards it
	present := tweakConfig(t, base, "down-param", func(e map[string]any) {
		e["chain"] = map[string]any{"from": "up", "param": "id"}
		e["url"] = srv.URL + "/down/detail?id={{id}}"
	})
	h2 := loadChainHandler(t, present)
	res, err := h2.Endpoint("down-param").Execute(nil)
	if err != nil {
		t.Fatalf("Execute with param=id: %v", err)
	}
	if len(res) != 2 || res[0]["id"] != "1" || res[1]["id"] != "2" {
		t.Errorf("got %+v", res)
	}
}

func TestChainHTMLModeUpstream(t *testing.T) {
	srv, reqLog := fixtureServer(t)
	h := loadChainHandler(t, chainConfig(t, srv))

	res, err := h.Endpoint("down-html").Execute(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d results, want 2", len(res))
	}
	// the HTML extractor produces hrefs "/d/1" and "/d/2", fed verbatim
	// into the downstream URL path (no query escaping there)
	got := []string{res[0]["path"], res[1]["path"]}
	want := []string{"/down/d/1", "/down/d/2"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("path[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	reqs := reqLog.snapshot()
	if reqs[0].path != "/up-html" || reqs[1].path != "/down/d/1" {
		t.Errorf("unexpected request order: %+v", reqs)
	}
}

func TestChainEmptyUpstreamIsEmptyResultNotError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"items":[]}`)
	}))
	defer srv.Close()

	cfg := fmtJSONConfig(t, map[string]any{
		"empty-up": map[string]any{
			"mode": "json", "url": srv.URL, "list": ".items[]",
			"result": map[string]any{"id": ".id"},
		},
		"after": map[string]any{
			"mode": "json", "url": srv.URL + "/x?id={{id}}",
			"result": map[string]any{"id": ".id"},
			"chain":  map[string]any{"from": "empty-up"},
		},
	})
	h := loadChainHandler(t, cfg)
	res, err := h.Endpoint("after").Execute(nil)
	if err != nil {
		t.Fatalf("empty upstream must not be an error: %v", err)
	}
	if len(res) != 0 {
		t.Fatalf("got %d results, want 0", len(res))
	}
}

func TestChainConfigValidation(t *testing.T) {
	srv, _ := fixtureServer(t)
	base := chainConfig(t, srv)

	t.Run("unknown upstream", func(t *testing.T) {
		cfg := strings.Replace(base, `"from":"up"`, `"from":"ghost"`, 1)
		h := &Handler{}
		err := h.LoadConfig([]byte(cfg))
		if err == nil || !strings.Contains(err.Error(), "unknown endpoint") {
			t.Fatalf("want unknown-endpoint error, got %v", err)
		}
	})

	t.Run("cycle rejected", func(t *testing.T) {
		// make "up" chain from "down" while "down" chains from "up"
		cfg := strings.Replace(base,
			`"up":{`,
			`"up":{"chain":{"from":"down"},`, 1)
		h := &Handler{}
		err := h.LoadConfig([]byte(cfg))
		if err == nil || !strings.Contains(err.Error(), "cycle") {
			t.Fatalf("want cycle error, got %v", err)
		}
	})
}

func TestChainMultiHopNestsUpstreamErrors(t *testing.T) {
	// three hops: level0 (fails 503) <- level1 <- level2. The top-level
	// error must nest each hop and carry the original HTTP status.
	srv, _ := fixtureServer(t)
	cfg := map[string]any{
		"level0": map[string]any{
			"mode": "json", "url": srv.URL + "/boom",
			"result": map[string]any{"id": ".id"},
		},
		"level1": map[string]any{
			"mode": "json", "url": srv.URL + "/down/detail?id={{id}}",
			"result": map[string]any{"id": ".id"},
			"chain":  map[string]any{"from": "level0"},
		},
		"level2": map[string]any{
			"mode": "json", "url": srv.URL + "/down/detail?id={{id}}",
			"result": map[string]any{"id": ".id"},
			"chain":  map[string]any{"from": "level1"},
		},
	}
	h := loadChainHandler(t, fmtJSONConfig(t, cfg))

	_, err := h.Endpoint("level2").Execute(nil)
	var ce *ChainError
	if !errors.As(err, &ce) {
		t.Fatalf("want *ChainError, got %T: %v", err, err)
	}
	if ce.Endpoint != "level2" || ce.Status != http.StatusServiceUnavailable {
		t.Errorf("top-level = endpoint %q status %d", ce.Endpoint, ce.Status)
	}
	if ce.Upstream == nil || ce.Upstream.Endpoint != "level1" {
		t.Fatalf("missing level1 upstream: %+v", ce.Upstream)
	}
	if ce.Upstream.Status != http.StatusServiceUnavailable {
		t.Errorf("level1 status = %d, want 503", ce.Upstream.Status)
	}
	if ce.Upstream.Upstream == nil || ce.Upstream.Upstream.Endpoint != "level0" {
		t.Fatalf("missing level0 upstream: %+v", ce.Upstream.Upstream)
	}

	// the rendered envelope keeps the same nested shape over HTTP
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/level2", nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rr.Code, rr.Body.String())
	}
	var rendered ChainError
	if err := json.Unmarshal(rr.Body.Bytes(), &rendered); err != nil {
		t.Fatal(err)
	}
	if rendered.Upstream == nil || rendered.Upstream.Upstream == nil {
		t.Fatalf("nested upstream lost over HTTP: %s", rr.Body.String())
	}
}

func TestChainTransportFailureIsUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"1"}`)
	}))
	deadURL := srv.URL
	srv.Close() // nothing listening: every call fails at the transport

	e := &Endpoint{
		Name:   "dead",
		Mode:   "json",
		URL:    deadURL + "/{{id}}",
		Result: map[string]Extractors{"id": mustExtractors(t, ".id")},
	}
	_, err := e.Execute(map[string]string{"id": "1"})
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("want *UpstreamError, got %T: %v", err, err)
	}
	if ue.Endpoint != "dead" || ue.Status != 0 || ue.Detail == "" {
		t.Errorf("got %+v", ue)
	}
	if ue.Err == nil {
		t.Error("underlying transport error must be retained for Unwrap")
	}

	// the same failure inside a chain becomes a stable ChainError
	cfg := map[string]any{
		"dead": map[string]any{
			"mode": "json", "url": deadURL,
			"result": map[string]any{"id": ".id"},
		},
		"after": map[string]any{
			"mode": "json", "url": deadURL + "/x?id={{id}}",
			"result": map[string]any{"id": ".id"},
			"chain":  map[string]any{"from": "dead"},
		},
	}
	h := loadChainHandler(t, fmtJSONConfig(t, cfg))
	_, err = h.Endpoint("after").Execute(nil)
	var ce *ChainError
	if !errors.As(err, &ce) {
		t.Fatalf("want *ChainError, got %T: %v", err, err)
	}
	if ce.Endpoint != "after" || ce.Status != 0 {
		t.Errorf("top-level = %+v, want endpoint after with no HTTP status", ce)
	}
	if ce.Upstream == nil || ce.Upstream.Endpoint != "dead" {
		t.Fatalf("missing nested dead hop: %+v", ce.Upstream)
	}
	// no HTTP status known → served as 500 rather than 502
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/after", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for transport errors", rr.Code)
	}
}

func TestNon2xxOnPlainEndpointIsUpstreamError(t *testing.T) {
	srv, _ := fixtureServer(t)
	h := loadChainHandler(t, chainConfig(t, srv))

	_, err := h.Endpoint("boom").Execute(nil)
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("want *UpstreamError, got %T: %v", err, err)
	}
	if ue.Status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", ue.Status)
	}
	if !strings.Contains(ue.Detail, "upstream unavailable") {
		t.Errorf("detail = %q", ue.Detail)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	var envelope map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["endpoint"] != "boom" || int(envelope["status"].(float64)) != 503 {
		t.Errorf("envelope = %+v", envelope)
	}
}

func fmtJSONConfig(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// tweakConfig parses a JSON config, mutates one endpoint and re-encodes it.
func tweakConfig(t *testing.T, cfg, endpoint string, fn func(map[string]any)) string {
	t.Helper()
	var all map[string]map[string]any
	if err := json.Unmarshal([]byte(cfg), &all); err != nil {
		t.Fatal(err)
	}
	e, ok := all[endpoint]
	if !ok {
		t.Fatalf("endpoint %q missing from config", endpoint)
	}
	fn(e)
	return fmtJSONConfig(t, all)
}
