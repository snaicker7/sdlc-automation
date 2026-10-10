package engine

import (
	"context"
	"strings"
	"testing"
)

// The git transport auth header is the token base64-encoded; a failed command must not echo it.
func TestShErrorDoesNotLeakAuthHeader(t *testing.T) {
	const secret = "eC1hY2Nlc3MtdG9rZW46U0VDUkVU"
	_, err := git(context.Background(), t.TempDir(),
		"-c", "http.https://github.com/.extraheader=AUTHORIZATION: basic "+secret, "definitely-not-a-git-command")
	if err == nil {
		t.Fatal("expected git to fail")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error message leaks the auth header: %v", err)
	}
	if !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("expected a redaction marker in: %v", err)
	}
}

func TestRedactArgsKeepsOtherArgs(t *testing.T) {
	got := redactArgs([]string{"push", "-u", "origin", "main"})
	if got != "push -u origin main" {
		t.Fatalf("got %q", got)
	}
}
