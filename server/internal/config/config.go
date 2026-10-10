// Package config reads shyd's environment.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const (
	defaultAddr   = ":8080"
	defaultDB     = "shy.db"
	defaultIngest = "https://ingest.neatlogs.com"
	defaultOpenAI = "https://openrouter.ai/api/v1"
	defaultBody   = 8 << 20
	maxBody       = 50 << 20
)

// Config is everything the process needs, with secrets left as the operator set them.
type Config struct {
	Addr              string
	DBPath            string
	PublicURL         string
	NeatlogsAPIKey    string
	NeatlogsProject   string
	NeatlogsIngestURL string
	OpenAIAPIKey      string
	OpenAIBaseURL     string
	BodyLimit         int64
}

// FromEnv loads configuration. A write key (nlw_…) without a project name is an error.
// An empty Neatlogs key is allowed: traces are stored and not forwarded.
// An empty OpenAI key is allowed: chat completions are rejected until it is set.
// SHY_OPENAI_BASE_URL defaults to OpenRouter's OpenAI-compatible root.
func FromEnv() (Config, error) {
	cfg := Config{
		Addr:              env("SHY_HTTP_ADDR", defaultAddr),
		DBPath:            env("SHY_DB_PATH", defaultDB),
		NeatlogsAPIKey:    strings.TrimSpace(os.Getenv("SHY_NEATLOGS_API_KEY")),
		NeatlogsProject:   strings.TrimSpace(os.Getenv("SHY_NEATLOGS_PROJECT_NAME")),
		NeatlogsIngestURL: env("SHY_NEATLOGS_INGEST_URL", defaultIngest),
		OpenAIAPIKey:      strings.TrimSpace(os.Getenv("SHY_OPENAI_API_KEY")),
		OpenAIBaseURL:     env("SHY_OPENAI_BASE_URL", defaultOpenAI),
	}
	var err error
	cfg.BodyLimit, err = envInt("SHY_BODY_LIMIT", defaultBody, maxBody)
	if err != nil {
		return Config{}, err
	}
	public := strings.TrimSpace(os.Getenv("SHY_PUBLIC_URL"))
	if public == "" {
		public = defaultPublic(cfg.Addr)
	}
	cfg.PublicURL, err = checkBaseURL(public, "SHY_PUBLIC_URL")
	if err != nil {
		return Config{}, err
	}
	cfg.NeatlogsIngestURL, err = checkBaseURL(cfg.NeatlogsIngestURL, "SHY_NEATLOGS_INGEST_URL")
	if err != nil {
		return Config{}, err
	}
	cfg.OpenAIBaseURL, err = checkAPIURL(cfg.OpenAIBaseURL, "SHY_OPENAI_BASE_URL")
	if err != nil {
		return Config{}, err
	}
	if err := checkToken(cfg.OpenAIAPIKey, "SHY_OPENAI_API_KEY"); err != nil {
		return Config{}, err
	}
	if strings.HasPrefix(cfg.NeatlogsAPIKey, "nlw_") && cfg.NeatlogsProject == "" {
		return Config{}, fmt.Errorf("SHY_NEATLOGS_PROJECT_NAME is required when SHY_NEATLOGS_API_KEY is a write key (nlw_)")
	}
	return cfg, nil
}

func defaultPublic(addr string) string {
	host := addr
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	return "http://" + host
}

func checkBaseURL(raw, name string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return "", fmt.Errorf("%s must be an http(s) URL", name)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s must not include a path, query, or fragment", name)
	}
	return strings.TrimRight(raw, "/"), nil
}

// checkAPIURL allows a path, because OpenAI-compatible roots include /v1.
func checkAPIURL(raw, name string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return "", fmt.Errorf("%s must be an http(s) URL", name)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s must not include a query or fragment", name)
	}
	return strings.TrimRight(raw, "/"), nil
}

func checkToken(raw, name string) error {
	for _, r := range raw {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("%s must not contain whitespace or control characters", name)
		}
	}
	return nil
}

func env(name, fallback string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	return v
}

func envInt(name string, fallback, max int64) (int64, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1024 {
		return 0, fmt.Errorf("%s must be a byte count of at least 1024", name)
	}
	if n > max {
		return 0, fmt.Errorf("%s must be at most %d", name, max)
	}
	return n, nil
}
