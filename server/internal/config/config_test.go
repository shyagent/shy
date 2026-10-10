package config

import "testing"

func TestFromEnvDefaults(t *testing.T) {
	t.Setenv("SHY_HTTP_ADDR", "")
	t.Setenv("SHY_DB_PATH", "")
	t.Setenv("SHY_PUBLIC_URL", "")
	t.Setenv("SHY_NEATLOGS_API_KEY", "")
	t.Setenv("SHY_NEATLOGS_PROJECT_NAME", "")
	t.Setenv("SHY_NEATLOGS_INGEST_URL", "")
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
