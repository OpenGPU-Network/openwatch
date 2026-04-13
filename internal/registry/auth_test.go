package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestNormalizeRegistryHost walks the Docker Hub alias table. Parse
// already converges on registry-1.docker.io, but LoadAuth accepts
// inputs from tests and future API callers that may pass any of the
// aliases — they must all collapse to the same canonical form.
func TestNormalizeRegistryHost(t *testing.T) {
	cases := map[string]string{
		"":                      defaultRegistry,
		"docker.io":             defaultRegistry,
		"index.docker.io":       defaultRegistry,
		"registry-1.docker.io":  defaultRegistry,
		"  DOCKER.IO ":          defaultRegistry,
		"ghcr.io":               "ghcr.io",
		"gcr.io":                "gcr.io",
		"my.private.registry":   "my.private.registry",
	}
	for in, want := range cases {
		if got := normalizeRegistryHost(in); got != want {
			t.Errorf("normalizeRegistryHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestECRHostPattern documents which hosts route through the ECR
// auth path and which do not. The pattern must match only
// <account>.dkr.ecr.<region>.amazonaws.com — anything else is a
// regular Docker-config lookup.
func TestECRHostPattern(t *testing.T) {
	match := []string{
		"123456789012.dkr.ecr.us-east-1.amazonaws.com",
		"987654321098.dkr.ecr.eu-west-2.amazonaws.com",
		"1.dkr.ecr.ap-southeast-1.amazonaws.com",
	}
	miss := []string{
		"ghcr.io",
		"docker.io",
		"dkr.ecr.us-east-1.amazonaws.com",      // no account
		"abc.dkr.ecr.us-east-1.amazonaws.com",  // non-numeric account
		"evil.dkr.ecr.us-east-1.amazonaws.com", // non-numeric account
		"12345.dkr.ecr..amazonaws.com",         // empty region
		"12345.dkr.ecr.us-east-1.amazonaws.com.attacker.com",
	}
	for _, h := range match {
		if !ecrHostPattern.MatchString(h) {
			t.Errorf("ecrHostPattern should match %q", h)
		}
	}
	for _, h := range miss {
		if ecrHostPattern.MatchString(h) {
			t.Errorf("ecrHostPattern should NOT match %q", h)
		}
	}
}

// TestLoadAuth_DockerConfigHit builds a temp config.json with a
// credential for ghcr.io and asserts LoadAuth returns it. This is
// the end-to-end generic-registry path — no ECR, no aliases, just
// "find user:pass for host in config.json".
func TestLoadAuth_DockerConfigHit(t *testing.T) {
	dir := t.TempDir()
	auth := base64.StdEncoding.EncodeToString([]byte("bob:secret"))
	cfg := map[string]any{
		"auths": map[string]any{
			"ghcr.io": map[string]any{"auth": auth},
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadAuth(context.Background(), dir, "ghcr.io", "", "")
	if err != nil {
		t.Fatalf("LoadAuth returned error: %v", err)
	}
	if got == nil {
		t.Fatal("LoadAuth returned nil auth for known host")
	}
	if got.Username != "bob" || got.Password != "secret" {
		t.Errorf("got user=%q pass=%q, want bob/secret", got.Username, got.Password)
	}
}

// TestLoadAuth_AnonymousOnMiss confirms that a host not present in
// config.json resolves to (nil, nil) — the "fall through to
// anonymous" state — rather than an error.
func TestLoadAuth_AnonymousOnMiss(t *testing.T) {
	dir := t.TempDir()
	cfg := map[string]any{"auths": map[string]any{}}
	data, _ := json.Marshal(cfg)
	_ = os.WriteFile(filepath.Join(dir, "config.json"), data, 0600)

	got, err := LoadAuth(context.Background(), dir, "ghcr.io", "", "")
	if err != nil {
		t.Fatalf("LoadAuth returned error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil auth for unknown host, got %+v", got)
	}
}
