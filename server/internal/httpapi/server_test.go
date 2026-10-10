package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"shy/server/internal/config"
	"shy/server/internal/httpapi"
	"shy/server/internal/neatlogs"
	"shy/server/internal/store"
)

func TestDashboardAndIngest(t *testing.T) {
	fwd := &recordingFwd{}
	ts, _ := newServer(t, fwd, config.Config{
		PublicURL:         "http://127.0.0.1:8080",
		NeatlogsAPIKey:    "project-key",
		NeatlogsIngestURL: "https://ingest.neatlogs.com",
		BodyLimit:         1 << 20,
	})
	client := newClient(t)

	page := createAccount(t, client, ts.URL)
	number := match(t, page, `data-copy="(\d{16})"`)
	if !strings.Contains(page, "Write it down") {
		t.Fatal("missing new-account banner")
	}

	keyPage := postForm(t, client, ts.URL+"/installs", url.Values{
		"csrf": {match(t, page, `name="csrf" value="([^"]+)"`)},
		"name": {"laptop"},
	})
	key := match(t, keyPage, `shy_[A-Za-z0-9_-]{40,}`)
	if !strings.Contains(keyPage, key) || !strings.Contains(keyPage, "export NEATLOGS_API_KEY="+key) {
		t.Fatal("install key was not revealed")
	}
	if strings.Contains(get(t, client, ts.URL+"/"), key) {
		t.Fatal("api key survived a reload")
	}

	payload := `{
		"name":"chat-turn",
		"project":"evil",
		"input":"fix the tests",
		"attributes":{"neatlogs.end_user.id":"spoof","neatlogs.session.id":"sess"},
		"children":[{"kind":"LLM","model":"example","tokens":{"prompt":12,"completion":3,"total":15}}]
	}`
	res := postJSON(t, ts.URL+"/v1/trace", key, payload)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("ingest %d %s", res.StatusCode, readAll(t, res))
	}
	var body map[string]any
	if err := json.Unmarshal(readAll(t, res), &body); err != nil {
		t.Fatal(err)
	}
	if body["trace_id"] != "0123456789abcdef0123456789abcdef" || body["forwarded"] != true {
		t.Fatalf("%v", body)
	}
	fwd.mu.Lock()
	forwarded := append([]byte(nil), fwd.body...)
	fwd.mu.Unlock()
	var root map[string]any
	if err := json.Unmarshal(forwarded, &root); err != nil {
		t.Fatal(err)
	}
	if _, ok := root["project"]; ok {
		t.Fatal("client project was forwarded")
	}
	attrs := root["attributes"].(map[string]any)
	if attrs["neatlogs.end_user.id"] != number {
		t.Fatalf("end user %v", attrs["neatlogs.end_user.id"])
	}

	dash := get(t, client, ts.URL+"/")
	if !strings.Contains(dash, "chat-turn") || !strings.Contains(dash, "sent") {
		t.Fatal("dashboard missing the trace")
	}
	detail := get(t, client, ts.URL+"/traces/"+match(t, dash, `/traces/(\d+)`))
	if !strings.Contains(detail, "0123456789abcdef0123456789abcdef") || !strings.Contains(detail, "example") {
		t.Fatal("detail missing neatlogs id or model")
	}

	other := createAccount(t, newClient(t), ts.URL)
	if strings.Contains(other, "chat-turn") {
		t.Fatal("trace leaked across accounts")
	}

	fresh := newClient(t)
	spaced := number[0:4] + " " + number[4:8] + " " + number[8:12] + " " + number[12:16]
	signed := postForm(t, fresh, ts.URL+"/login", url.Values{
		"form":   {formToken(t, fresh, ts.URL)},
		"number": {spaced},
	})
	if !strings.Contains(signed, "laptop") {
		t.Fatal("spaced account number did not sign in")
	}

	id := match(t, signed, `action="/installs/(\d+)/revoke"`)
	postForm(t, fresh, ts.URL+"/installs/"+id+"/revoke", url.Values{
		"csrf": {match(t, signed, `name="csrf" value="([^"]+)"`)},
	})
	revoked := postJSON(t, ts.URL+"/v1/trace", key, payload)
	if revoked.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked key status %d", revoked.StatusCode)
	}
	revoked.Body.Close()

	denied, err := client.PostForm(ts.URL+"/installs", url.Values{"csrf": {"not-the-token"}, "name": {"x"}})
	if err != nil {
		t.Fatal(err)
	}
	defer denied.Body.Close()
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("bad csrf status %d", denied.StatusCode)
	}
}

func TestIngestFailures(t *testing.T) {
	fwd := &recordingFwd{fail: errBoom{}}
	ts, _ := newServer(t, fwd, config.Config{
		PublicURL:         "http://127.0.0.1:8080",
		NeatlogsAPIKey:    "project-key",
		NeatlogsIngestURL: "https://ingest.neatlogs.com",
		BodyLimit:         1 << 20,
	})
	client := newClient(t)
	page := createAccount(t, client, ts.URL)
	keyPage := postForm(t, client, ts.URL+"/installs", url.Values{
		"csrf": {match(t, page, `name="csrf" value="([^"]+)"`)},
		"name": {"box"},
	})
	key := match(t, keyPage, `shy_[A-Za-z0-9_-]{40,}`)

	res := postJSON(t, ts.URL+"/v1/trace", key, `{"name":"chat-turn"}`)
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d %s", res.StatusCode, readAll(t, res))
	}
	res.Body.Close()
	if !strings.Contains(get(t, client, ts.URL+"/"), "failed") {
		t.Fatal("dashboard did not show the failed forward")
	}

	missing := postJSON(t, ts.URL+"/v1/trace", key, `{"status":"OK"}`)
	if missing.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d %s", missing.StatusCode, readAll(t, missing))
	}
	missing.Body.Close()

	none := postJSON(t, ts.URL+"/v1/trace", "", `{"name":"anon-turn","attributes":{"neatlogs.end_user.id":"spoof"}}`)
	if none.StatusCode != http.StatusBadGateway {
		t.Fatalf("anonymous status %d %s", none.StatusCode, readAll(t, none))
	}
	none.Body.Close()
	fwd.mu.Lock()
	anonBody := string(fwd.body)
	fwd.mu.Unlock()
	if !strings.Contains(anonBody, `"neatlogs.end_user.id":"anonymous"`) {
		t.Fatalf("anonymous identity %s", anonBody)
	}
	if strings.Contains(get(t, client, ts.URL+"/"), "anon-turn") {
		t.Fatal("anonymous trace appeared on an account dashboard")
	}

	bad := postJSON(t, ts.URL+"/v1/trace", "not-a-real-key", `{"name":"x"}`)
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad key status %d", bad.StatusCode)
	}
	bad.Body.Close()
}

func TestStoreOnly(t *testing.T) {
	ts, _ := newServer(t, neatlogs.Disabled{}, config.Config{
		PublicURL: "http://127.0.0.1:8080",
		BodyLimit: 1 << 20,
	})
	client := newClient(t)
	page := createAccount(t, client, ts.URL)
	keyPage := postForm(t, client, ts.URL+"/installs", url.Values{
		"csrf": {match(t, page, `name="csrf" value="([^"]+)"`)},
	})
	key := match(t, keyPage, `shy_[A-Za-z0-9_-]{40,}`)
	res := postJSON(t, ts.URL+"/v1/trace", key, `{"name":"chat-turn"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", res.StatusCode, readAll(t, res))
	}
	res.Body.Close()
	anon := postJSON(t, ts.URL+"/v1/trace", "", `{"name":"anon-turn"}`)
	if anon.StatusCode != http.StatusOK {
		t.Fatalf("anonymous status %d %s", anon.StatusCode, readAll(t, anon))
	}
	var body map[string]any
	if err := json.Unmarshal(readAll(t, anon), &body); err != nil {
		t.Fatal(err)
	}
	if body["stored"] != true || body["forwarded"] != false {
		t.Fatalf("%v", body)
	}
	page = get(t, client, ts.URL+"/")
	if !strings.Contains(page, "skipped") || strings.Contains(page, "anon-turn") {
		t.Fatal("anonymous trace was mixed into the account list")
	}
}

func TestRegisterRateLimit(t *testing.T) {
	ts := httptest.NewServer(mustHandler(t, neatlogs.Disabled{}, config.Config{
		PublicURL: "http://127.0.0.1:8080",
		BodyLimit: 1 << 20,
	}, 20, 2))
	t.Cleanup(ts.Close)
	client := newClient(t)
	tok := formToken(t, client, ts.URL)
	var last int
	for i := 0; i < 3; i++ {
		res, err := client.PostForm(ts.URL+"/accounts", url.Values{"form": {tok}})
		if err != nil {
			t.Fatal(err)
		}
		last = res.StatusCode
		res.Body.Close()
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("last status %d", last)
	}
}

func TestHealth(t *testing.T) {
	ts, _ := newServer(t, neatlogs.Disabled{}, config.Config{
		PublicURL: "http://127.0.0.1:8080",
		BodyLimit: 1 << 20,
	})
	res, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || string(readAll(t, res)) != "ok\n" {
		t.Fatalf("status %d", res.StatusCode)
	}
	res, err = http.Get(ts.URL + "/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(readAll(t, res)), "color-scheme") {
		t.Fatalf("css status %d", res.StatusCode)
	}
}

type recordingFwd struct {
	mu   sync.Mutex
	body []byte
	fail error
}

func (r *recordingFwd) Forward(_ context.Context, body []byte) (string, int, error) {
	r.mu.Lock()
	r.body = append([]byte(nil), body...)
	r.mu.Unlock()
	if r.fail != nil {
		return "", 0, r.fail
	}
	return "0123456789abcdef0123456789abcdef", 2, nil
}

type errBoom struct{}

func (errBoom) Error() string { return "neatlogs http 400: no" }

func newServer(t *testing.T, fwd neatlogs.Forwarder, cfg config.Config) (*httptest.Server, *store.Store) {
	t.Helper()
	h, st := mustHandlerStore(t, fwd, cfg, 20, 10)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts, st
}

func mustHandler(t *testing.T, fwd neatlogs.Forwarder, cfg config.Config, login, register int) http.Handler {
	t.Helper()
	h, _ := mustHandlerStore(t, fwd, cfg, login, register)
	return h
}

func mustHandlerStore(t *testing.T, fwd neatlogs.Forwarder, cfg config.Config, login, register int) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "shy.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	app, err := httpapi.New(httpapi.Options{
		Config:        cfg,
		Store:         st,
		Forward:       fwd,
		LoginLimit:    login,
		RegisterLimit: register,
	})
	if err != nil {
		t.Fatal(err)
	}
	return app.Handler(), st
}

func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

func createAccount(t *testing.T, client *http.Client, base string) string {
	t.Helper()
	return postForm(t, client, base+"/accounts", url.Values{"form": {formToken(t, client, base)}})
}

func formToken(t *testing.T, client *http.Client, base string) string {
	t.Helper()
	return match(t, get(t, client, base+"/"), `name="form" value="([^"]+)"`)
}

func get(t *testing.T, client *http.Client, rawURL string) string {
	t.Helper()
	res, err := client.Get(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", rawURL, res.StatusCode, readAll(t, res))
	}
	return string(readAll(t, res))
}

func postForm(t *testing.T, client *http.Client, rawURL string, vals url.Values) string {
	t.Helper()
	res, err := client.PostForm(rawURL, vals)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b := readAll(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: %d %s", rawURL, res.StatusCode, b)
	}
	return string(b)
}

func postJSON(t *testing.T, rawURL, key, payload string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, rawURL, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func readAll(t *testing.T, res *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func match(t *testing.T, s, expr string) string {
	t.Helper()
	m := regexp.MustCompile(expr).FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("no match for %s", expr)
	}
	if len(m) == 1 {
		return m[0]
	}
	return m[1]
}
