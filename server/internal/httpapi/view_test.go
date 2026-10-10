package httpapi

import (
	"strings"
	"testing"
)

func TestMaskNumber(t *testing.T) {
	if got := maskNumber("1111222233334444"); got != "•••• •••• •••• 4444" {
		t.Fatal(got)
	}
	raw := `{"neatlogs.end_user.id":"1111222233334444","note":"1111 2222 3333 4444"}`
	got := redactAccount(raw, "1111222233334444")
	if strings.Contains(got, "1111222233334444") || strings.Contains(got, "1111 2222 3333 4444") {
		t.Fatal(got)
	}
	if !strings.Contains(got, "••••••••••••4444") || !strings.Contains(got, "•••• •••• •••• 4444") {
		t.Fatal(got)
	}
}

func TestRunLine(t *testing.T) {
	key := "shy_abc"
	if got := runLine("http://127.0.0.1:8080", key); got != "curl github.com/shyagent/shy/shy.sh > shy.sh && SHY_API_KEY="+key+" ./shy.sh" {
		t.Fatal(got)
	}
	if got := runLine("http://shy.example", key); got != "curl github.com/shyagent/shy/shy.sh > shy.sh && SHY_BASE_URL=http://shy.example SHY_API_KEY="+key+" ./shy.sh" {
		t.Fatal(got)
	}
	if got := runLine("http://shy.example", "shy_it's"); got != "curl github.com/shyagent/shy/shy.sh > shy.sh && SHY_BASE_URL=http://shy.example SHY_API_KEY='shy_it'\\''s' ./shy.sh" {
		t.Fatalf("%q", got)
	}
	if got := shellWord(""); got != "''" {
		t.Fatal(got)
	}
}
