package httpapi_test

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"shy/server/internal/config"
	"shy/server/internal/neatlogs"
)

func TestChatForward(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotAuth, gotTitle, gotReferer, gotAPIKey, gotBody, gotAccept string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath = r.URL.RequestURI()
		gotAuth = r.Header.Get("Authorization")
		gotTitle = r.Header.Get("X-Title")
		gotReferer = r.Header.Get("HTTP-Referer")
		gotAPIKey = r.Header.Get("X-Api-Key")
		gotAccept = r.Header.Get("Accept")
		gotBody = string(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl","choices":[{"message":{"content":"hi"}}]}`))
	}))
	t.Cleanup(upstream.Close)

	ts, _ := newServer(t, neatlogs.Disabled{}, config.Config{
		PublicURL:     "http://shy.example",
		OpenAIBaseURL: upstream.URL + "/v1",
		OpenAIAPIKey:  "upstream-secret",
		BodyLimit:     1 << 20,
	})
	client := newClient(t)
	page := createAccount(t, client, ts.URL)
	keyPage := postForm(t, client, ts.URL+"/installs", url.Values{
		"csrf": {match(t, page, `name="csrf" value="([^"]+)"`)},
		"name": {"laptop"},
	})
	key := match(t, keyPage, `shy_[A-Za-z0-9_-]{40,}`)
	if !strings.Contains(keyPage, "export SHY_API_KEY="+key) || !strings.Contains(keyPage, "export SHY_BASE_URL=http://shy.example") {
		t.Fatal("install env did not include the chat settings")
	}
	wantCmd := "curl github.com/shyagent/shy/shy.sh &gt; shy.sh &amp;&amp; SHY_BASE_URL=http://shy.example SHY_API_KEY=" + key + " ./shy.sh"
	if !strings.Contains(keyPage, wantCmd) {
		t.Fatal("run command did not point at this server")
	}
	if !strings.Contains(get(t, client, ts.URL+"/"), "—") {
		t.Fatal("expected an unused install")
	}

	payload := `{"model":"deepseek/deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`
	res := postChat(t, ts.URL+"/v1/chat/completions", "Bearer "+key, payload)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", res.StatusCode, readAll(t, res))
	}
	if string(readAll(t, res)) != `{"id":"cmpl","choices":[{"message":{"content":"hi"}}]}` {
		t.Fatal("response was not passed through")
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/v1/chat/completions" || gotAuth != "Bearer upstream-secret" || gotBody != payload {
		t.Fatalf("path %q auth %q body %q", gotPath, gotAuth, gotBody)
	}
	if gotTitle != "shy" || gotReferer != "http://shy.example" || gotAPIKey != "" || gotAccept != "application/json" {
		t.Fatalf("title %q referer %q apikey %q accept %q", gotTitle, gotReferer, gotAPIKey, gotAccept)
	}
	if strings.Contains(gotAuth, key) {
		t.Fatal("install key was forwarded")
	}
	if strings.Contains(get(t, client, ts.URL+"/"), "—") {
		t.Fatal("install last seen was not updated")
	}
}

func TestChatAuthAndBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	t.Cleanup(upstream.Close)

	ts, _ := newServer(t, neatlogs.Disabled{}, config.Config{
		PublicURL:     "http://127.0.0.1:8080",
		OpenAIBaseURL: upstream.URL + "/v1",
		OpenAIAPIKey:  "upstream-secret",
		BodyLimit:     64,
	})
	client := newClient(t)
	page := createAccount(t, client, ts.URL)
	keyPage := postForm(t, client, ts.URL+"/installs", url.Values{
		"csrf": {match(t, page, `name="csrf" value="([^"]+)"`)},
		"name": {"box"},
	})
	key := match(t, keyPage, `shy_[A-Za-z0-9_-]{40,}`)

	missing := postChat(t, ts.URL+"/v1/chat/completions", "", `{"model":"m"}`)
	if missing.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing key %d", missing.StatusCode)
	}
	missing.Body.Close()

	bad := postChat(t, ts.URL+"/v1/chat/completions", "Bearer not-a-real-key", `{"model":"m"}`)
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad key %d", bad.StatusCode)
	}
	bad.Body.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Api-Key", "shy_different")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("mismatched keys %d", res.StatusCode)
	}
	res.Body.Close()

	viaHeader := postJSON(t, ts.URL+"/v1/chat/completions", key, `{"model":"m"}`)
	if viaHeader.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("x-api-key status %d %s", viaHeader.StatusCode, readAll(t, viaHeader))
	}
	if string(readAll(t, viaHeader)) != `{"error":{"message":"slow down"}}` {
		t.Fatal("upstream error was not passed through")
	}

	badJSON := postChat(t, ts.URL+"/v1/chat/completions", "Bearer "+key, `[1]`)
	if badJSON.StatusCode != http.StatusBadRequest {
		t.Fatalf("array body %d", badJSON.StatusCode)
	}
	badJSON.Body.Close()

	tooBig := postChat(t, ts.URL+"/v1/chat/completions", "Bearer "+key, `{"model":"`+strings.Repeat("a", 80)+`"}`)
	if tooBig.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize %d %s", tooBig.StatusCode, readAll(t, tooBig))
	}
	tooBig.Body.Close()

	id := match(t, get(t, client, ts.URL+"/"), `action="/installs/(\d+)/revoke"`)
	postForm(t, client, ts.URL+"/installs/"+id+"/revoke", url.Values{
		"csrf": {match(t, get(t, client, ts.URL+"/"), `name="csrf" value="([^"]+)"`)},
	})
	revoked := postChat(t, ts.URL+"/v1/chat/completions", "Bearer "+key, `{"model":"m"}`)
	if revoked.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked %d", revoked.StatusCode)
	}
	revoked.Body.Close()
}

func TestChatUpstreamFailure(t *testing.T) {
	offSrv, offKey := chatInstall(t, config.Config{
		PublicURL: "http://127.0.0.1:8080",
		BodyLimit: 1 << 20,
	})
	off := postChat(t, offSrv.URL+"/v1/chat/completions", "Bearer "+offKey, `{"model":"m"}`)
	if off.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured %d %s", off.StatusCode, readAll(t, off))
	}
	off.Body.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	down, downKey := chatInstall(t, config.Config{
		PublicURL:     "http://127.0.0.1:8080",
		OpenAIBaseURL: "http://" + addr + "/v1",
		OpenAIAPIKey:  "upstream-secret",
		BodyLimit:     1 << 20,
	})
	refused := postChat(t, down.URL+"/v1/chat/completions", "Bearer "+downKey, `{"model":"m"}`)
	if refused.StatusCode != http.StatusBadGateway {
		t.Fatalf("refused %d %s", refused.StatusCode, readAll(t, refused))
	}
	refused.Body.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.com/steal", http.StatusFound)
	}))
	t.Cleanup(redirect.Close)
	moved, movedKey := chatInstall(t, config.Config{
		PublicURL:     "http://127.0.0.1:8080",
		OpenAIBaseURL: redirect.URL + "/v1",
		OpenAIAPIKey:  "upstream-secret",
		BodyLimit:     1 << 20,
	})
	hop := postChat(t, moved.URL+"/v1/chat/completions", "Bearer "+movedKey, `{"model":"m"}`)
	if hop.StatusCode != http.StatusBadGateway {
		t.Fatalf("redirect %d %s", hop.StatusCode, readAll(t, hop))
	}
	hop.Body.Close()
}

func chatInstall(t *testing.T, cfg config.Config) (*httptest.Server, string) {
	t.Helper()
	ts, _ := newServer(t, neatlogs.Disabled{}, cfg)
	client := newClient(t)
	page := createAccount(t, client, ts.URL)
	keyPage := postForm(t, client, ts.URL+"/installs", url.Values{
		"csrf": {match(t, page, `name="csrf" value="([^"]+)"`)},
	})
	return ts, match(t, keyPage, `shy_[A-Za-z0-9_-]{40,}`)
}

func postChat(t *testing.T, rawURL, auth, payload string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	res, err := (&http.Client{Transport: &http.Transport{DisableKeepAlives: true}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}
