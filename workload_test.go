package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fuss/history"
)

func TestHTTPKVStatusMapping(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, "v1\n")
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/fail", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	})
	mux.HandleFunc("/hang", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := &http.Client{Timeout: 200 * time.Millisecond}

	status, val, absent := applyHTTPOp(context.Background(), client, http.MethodPut, srv.URL+"/ok", "v1", history.OpPut)
	if status != history.StatusOK || absent {
		t.Fatalf("put ok: %s absent=%v", status, absent)
	}
	status, val, absent = applyHTTPOp(context.Background(), client, http.MethodGet, srv.URL+"/ok", "", history.OpGet)
	if status != history.StatusOK || absent || val != "v1" {
		t.Fatalf("get ok: %s %q absent=%v", status, val, absent)
	}
	status, _, absent = applyHTTPOp(context.Background(), client, http.MethodGet, srv.URL+"/missing", "", history.OpGet)
	if status != history.StatusOK || !absent {
		t.Fatalf("get 404: %s absent=%v", status, absent)
	}
	status, _, _ = applyHTTPOp(context.Background(), client, http.MethodPut, srv.URL+"/fail", "v", history.OpPut)
	if status != history.StatusFail {
		t.Fatalf("put 500: %s", status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	status, _, _ = applyHTTPOp(ctx, client, http.MethodGet, srv.URL+"/hang", "", history.OpGet)
	if status != history.StatusInfo {
		t.Fatalf("timeout: %s", status)
	}
}

func TestHTTPResetIsInfo(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	status, _, _ := applyHTTPOp(ctx, &http.Client{Timeout: time.Second}, http.MethodGet, "http://"+ln.Addr().String()+"/k/k0", "", history.OpGet)
	if status != history.StatusInfo {
		t.Fatalf("reset: %s", status)
	}
}

func TestHTTPJSONValueAndRedirect(t *testing.T) {
	var saw string
	mux := http.NewServeMux()
	mux.HandleFunc("/v3/kv/range", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "type", http.StatusBadRequest)
			return
		}
		switch string(b) {
		case `{"key":"azA="}`:
			_, _ = io.WriteString(w, `{"kvs":[{"value":"djE="}],"count":"1"}`)
		case `{"key":"bWlzcw=="}`:
			_, _ = io.WriteString(w, `{"header":{"revision":"2"}}`)
		case `{"key":"bad"}`:
			_, _ = io.WriteString(w, `{"results":[{"error":"bad"}]}`)
		default:
			http.Error(w, "unexpected "+string(b), http.StatusBadRequest)
		}
	})
	mux.HandleFunc("/openraft", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("/leader", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		saw = r.Method + " " + string(b)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/follow", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/leader", http.StatusMovedPermanently)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := &http.Client{Timeout: time.Second}

	status, val, absent := applyHTTPCall(context.Background(), client, httpCall{
		Method:        http.MethodPost,
		URL:           srv.URL + "/v3/kv/range",
		Body:          `{"key":"azA="}`,
		Headers:       map[string]string{"Content-Type": "application/json"},
		ValueFrom:     "kvs.0.value",
		ValueEncoding: "base64",
	}, history.OpGet)
	if status != history.StatusOK || absent || val != "v1" {
		t.Fatalf("json get: %s %q absent=%v", status, val, absent)
	}
	status, val, absent = applyHTTPCall(context.Background(), client, httpCall{
		Method:    http.MethodPost,
		URL:       srv.URL + "/v3/kv/range",
		Body:      `{"key":"bWlzcw=="}`,
		Headers:   map[string]string{"Content-Type": "application/json"},
		ValueFrom: "kvs.0.value",
	}, history.OpGet)
	if status != history.StatusOK || !absent || val != "" {
		t.Fatalf("missing json: %s %q absent=%v", status, val, absent)
	}
	status, _, _ = applyHTTPCall(context.Background(), client, httpCall{
		Method:    http.MethodPost,
		URL:       srv.URL + "/v3/kv/range",
		Body:      `{"key":"bad"}`,
		Headers:   map[string]string{"Content-Type": "application/json"},
		ErrorFrom: "results.0.error",
	}, history.OpPut)
	if status != history.StatusFail {
		t.Fatalf("error_from: %s", status)
	}
	for _, body := range []string{
		`{"Err":{"QuorumNotEnough":{"cluster":"voters","got":[]}}}`,
		`{"Err":{"LogEntryDiscarded":{"reason":"NotLeader"}}}`,
		`{"Err":{"ForwardToLeader":{"reason":"NotLeader"}}}`,
	} {
		status, _, _ = applyHTTPCall(context.Background(), client, httpCall{
			Method:    http.MethodPost,
			URL:       srv.URL + "/openraft",
			Body:      body,
			Headers:   map[string]string{"Content-Type": "application/json"},
			ErrorFrom: "Err",
		}, history.OpPut)
		if status != history.StatusFail {
			t.Fatalf("error_from object %s: %s", body, status)
		}
	}
	for _, body := range []string{`{"Ok":{"value":null}}`, `{"Err":null}`, `{"Err":""}`} {
		status, _, _ = applyHTTPCall(context.Background(), client, httpCall{
			Method:    http.MethodPost,
			URL:       srv.URL + "/openraft",
			Body:      body,
			Headers:   map[string]string{"Content-Type": "application/json"},
			ErrorFrom: "Err",
		}, history.OpPut)
		if status != history.StatusOK {
			t.Fatalf("error_from not a refusal %s: %s", body, status)
		}
	}
	status, _, _ = applyHTTPCall(context.Background(), client, httpCall{
		Method: http.MethodPost,
		URL:    srv.URL + "/follow",
		Body:   `{"a":1}`,
	}, history.OpPut)
	if status != history.StatusOK || saw != `POST {"a":1}` {
		t.Fatalf("redirect: status=%s saw=%q", status, saw)
	}

	vars := map[string]string{"key": "k0", "value": "v1", "old": ""}
	addKVEncodings(vars)
	got := expandWorkload(`{"key":"${key_b64}","value":"${value_b64}"}`, vars)
	if got != `{"key":"azA=","value":"djE="}` {
		t.Fatalf("b64 template: %s", got)
	}
}

func TestHTTPValueFromExpandsKey(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/key/k0", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"k0":"v1-0"}`)
	})
	mux.HandleFunc("/key/missing", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"missing":""}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	y := &YAMLAdapter{cfg: yamlTarget{Workload: yamlWorkload{
		Get: yamlKVCall{
			Method:    http.MethodGet,
			URL:       srv.URL + "/key/${key}",
			ValueFrom: "${key}",
		},
	}}}
	ctx := context.Background()
	client := &http.Client{Timeout: time.Second}
	host := strings.TrimPrefix(srv.URL, "http://")
	status, val, absent := y.doOp(ctx, nil, client, history.OpGet, "k0", "v1-0", "", host, 1)
	if status != history.StatusOK || absent || val != "v1-0" {
		t.Fatalf("dynamic key: %s %q absent=%v", status, val, absent)
	}
	status, val, absent = y.doOp(ctx, nil, client, history.OpGet, "missing", "", "", host, 1)
	if status != history.StatusOK || !absent || val != "" {
		t.Fatalf("empty dynamic value: %s %q absent=%v", status, val, absent)
	}
}

func TestExecOpStatus(t *testing.T) {
	ctx := context.Background()
	status, val, absent := applyExecOp(ctx, []string{"/bin/sh", "-c", "printf 'found'"}, history.OpGet)
	if status != history.StatusOK || absent || val != "found" {
		t.Fatalf("exec get: %s %q absent=%v", status, val, absent)
	}
	status, _, absent = applyExecOp(ctx, []string{"/bin/sh", "-c", "exit 0"}, history.OpGet)
	if status != history.StatusOK || !absent {
		t.Fatalf("exec empty get: %s absent=%v", status, absent)
	}
	status, _, _ = applyExecOp(ctx, []string{"/bin/sh", "-c", "exit 3"}, history.OpPut)
	if status != history.StatusFail {
		t.Fatalf("exec fail: %s", status)
	}
	tctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	status, _, _ = applyExecOp(tctx, []string{"/bin/sleep", "2"}, history.OpPut)
	if status != history.StatusInfo {
		t.Fatalf("exec timeout: %s", status)
	}
}

func TestRunCoreChecks(t *testing.T) {
	lost := []history.OpRecord{
		{Process: 1, Op: history.OpPut, Key: "k", Value: "a", CallNS: 0, ReturnNS: 10, Status: history.StatusOK},
		{Process: 2, Op: history.OpGet, Key: "k", Value: "", CallNS: 20, ReturnNS: 30, Status: history.StatusOK},
	}
	dir := t.TempDir()
	cr, html := runCoreChecks(coreSpec{
		ops:              lost,
		obs:              []history.Observation{{Key: "k", Absent: true, Status: history.StatusOK}},
		porcupine:        true,
		porcupineTimeout: time.Second,
		set:              true,
		htmlPath:         dir,
	})
	if cr.Verdict != "NONLINEARIZABLE" {
		t.Fatalf("verdict %s", cr.Verdict)
	}
	if len(html) == 0 {
		t.Fatal("no html bytes")
	}
	if _, err := os.Stat(filepath.Join(dir, "porcupine.html")); err != nil {
		t.Fatal(err)
	}

	cr, html = runCoreChecks(coreSpec{
		ops: lost,
		obs: []history.Observation{{Key: "k", Absent: true, Status: history.StatusOK}},
		set: true,
	})
	if cr.Verdict != "LOST WRITE" || len(html) != 0 {
		t.Fatalf("set verdict %s html %d", cr.Verdict, len(html))
	}

	cr, _ = runCoreChecks(coreSpec{
		ops: []history.OpRecord{
			{Process: 1, Op: history.OpGet, Key: "k", CallNS: 100, Status: history.StatusFail},
			{Process: 1, Op: history.OpGet, Key: "k", CallNS: 110, Status: history.StatusFail},
		},
		eventually:  true,
		eventuallyN: 1,
		quietStart:  100,
		quietEnd:    200,
	})
	if cr.Verdict != "UNAVAILABLE" {
		t.Fatalf("eventually %s", cr.Verdict)
	}

	okOps := []history.OpRecord{
		{Process: 1, Op: history.OpPut, Key: "k", Value: "a", CallNS: 100, ReturnNS: 110, Status: history.StatusOK},
		{Process: 2, Op: history.OpGet, Key: "k", Value: "a", CallNS: 120, ReturnNS: 130, Status: history.StatusOK},
	}
	cr, html = runCoreChecks(coreSpec{
		ops:              okOps,
		obs:              []history.Observation{{Key: "k", Value: "a", Status: history.StatusOK}},
		quietStart:       100,
		quietEnd:         200,
		porcupine:        true,
		porcupineTimeout: time.Second,
		set:              true,
		eventually:       true,
		eventuallyN:      1,
	})
	if cr.Verdict != "PASS" || cr.Evidence == 0 || len(html) != 0 {
		t.Fatalf("pass verdict %s evidence %d html %d", cr.Verdict, cr.Evidence, len(html))
	}
}
