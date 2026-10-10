package config

import "testing"

func TestFromEnvDefaults(t *testing.T) {
	t.Setenv("SHY_HTTP_ADDR", "")
	t.Setenv("SHY_DB_PATH", "")
	t.Setenv("SHY_PUBLIC_URL", "")
	t.Setenv("SHY_NEATLOGS_API_KEY", "")
	t.Setenv("SHY_NEATLOGS_PROJECT_NAME", "")
	t.Setenv("SHY_NEATLOGS_INGEST_URL", "")
	t.Setenv("SHY_OPENAI_API_KEY", "")
	t.Setenv("SHY_OPENAI_BASE_URL", "")
	t.Setenv("SHY_BODY_LIMIT", "")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":8080" || cfg.PublicURL != "http://127.0.0.1:8080" {
		t.Fatalf("%+v", cfg)
	}
	if cfg.NeatlogsIngestURL != "https://ingest.neatlogs.com" || cfg.BodyLimit != defaultBody {
		t.Fatalf("%+v", cfg)
	}
	if cfg.OpenAIAPIKey != "" || cfg.OpenAIBaseURL != defaultOpenAI {
		t.Fatalf("%+v", cfg)
	}
}

func TestOpenAIUpstream(t *testing.T) {
	t.Setenv("SHY_OPENAI_BASE_URL", "https://openrouter.ai/api/v1/")
	t.Setenv("SHY_OPENAI_API_KEY", "sk-test")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenAIBaseURL != "https://openrouter.ai/api/v1" || cfg.OpenAIAPIKey != "sk-test" {
		t.Fatalf("%+v", cfg)
	}

	t.Setenv("SHY_OPENAI_BASE_URL", "https://user:pass@openrouter.ai/api/v1")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected userinfo to be rejected")
	}
	t.Setenv("SHY_OPENAI_BASE_URL", "https://openrouter.ai/api/v1?x=1")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected query to be rejected")
	}
	t.Setenv("SHY_OPENAI_BASE_URL", defaultOpenAI)
	t.Setenv("SHY_OPENAI_API_KEY", "bad key")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected whitespace in the key to be rejected")
	}
}

func TestWriteKeyNeedsProject(t *testing.T) {
	t.Setenv("SHY_NEATLOGS_API_KEY", "nlw_abc")
	t.Setenv("SHY_NEATLOGS_PROJECT_NAME", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("expected error")
	}
	t.Setenv("SHY_NEATLOGS_PROJECT_NAME", "demo")
	cfg, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NeatlogsProject != "demo" {
		t.Fatal(cfg.NeatlogsProject)
	}
}
