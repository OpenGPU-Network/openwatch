package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ecr"

	"github.com/docker/docker/api/types/registry"
)

// ecrHostPattern matches any Amazon ECR private registry host of the form
// <account>.dkr.ecr.<region>.amazonaws.com. We match the SHAPE of the host
// and not the contents so we never log or store the account ID — ECR
// detection is purely a routing decision.
var ecrHostPattern = regexp.MustCompile(`^[0-9]+\.dkr\.ecr\.[a-z0-9-]+\.amazonaws\.com$`)

// ecrAuthTimeout is the wall-clock budget for a single
// GetAuthorizationToken call. ECR is normally fast (sub-second) but a
// misconfigured AWS_REGION or a network partition can otherwise stall
// the update loop for the default SDK retry duration.
const ecrAuthTimeout = 15 * time.Second

// dockerConfig mirrors the subset of ~/.docker/config.json we care about.
type dockerConfig struct {
	Auths map[string]dockerConfigAuth `json:"auths"`
}

type dockerConfigAuth struct {
	Auth     string `json:"auth"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// LoadAuth returns the credentials to use against the given registry host.
// The resolution order runs per-registry kind so each host type uses the
// most idiomatic auth mechanism:
//
//  1. Amazon ECR (host matches *.dkr.ecr.*.amazonaws.com) →
//     AWS SDK GetAuthorizationToken, reading credentials from the
//     standard AWS_* environment variables / shared config.
//  2. Any other host (Docker Hub, GHCR, GCR, generic private registry) →
//     Docker config.json at configPath or the default location, then
//     anonymous if nothing matches.
//
// A nil return value with nil error means "no credentials found, fall back
// to anonymous access" — callers should treat that as a valid state.
//
// All error messages are deliberately generic: we never log or return
// usernames, passwords, bearer tokens, or anything else that could leak
// through a centralized error-reporting pipeline.
func LoadAuth(ctx context.Context, configPath, registryHost string) (*registry.AuthConfig, error) {
	host := normalizeRegistryHost(registryHost)

	if ecrHostPattern.MatchString(host) {
		auth, err := loadECRAuth(ctx, host)
		if err != nil {
			// Never include the raw SDK error content — AWS errors can
			// contain account IDs and request metadata. Return a stable
			// sanitized string so callers may log it freely.
			return nil, errors.New("ecr authorization failed: check AWS_REGION and AWS_* credentials")
		}
		return auth, nil
	}

	return loadDockerConfigAuth(configPath, host)
}

// normalizeRegistryHost collapses the several Docker Hub aliases onto
// the canonical v2 registry host. Callers elsewhere already parse
// images through resolver.Parse, which emits "registry-1.docker.io"
// as the canonical form, but LoadAuth is also reachable from tests
// and future API callers — so we centralize the alias table here.
func normalizeRegistryHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	switch h {
	case "", "docker.io", "index.docker.io", "registry-1.docker.io":
		return defaultRegistry
	}
	return h
}

// loadECRAuth fetches a short-lived auth token from AWS ECR via
// GetAuthorizationToken. The returned token is base64 "user:password",
// which we split back out into the *registry.AuthConfig the rest of
// OpenWatch consumes. The AWS SDK reads credentials from the standard
// chain (env vars AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY /
// AWS_SESSION_TOKEN / AWS_REGION, shared config files, IMDS, etc.);
// OpenWatch does not implement its own AWS auth flow.
func loadECRAuth(parent context.Context, host string) (*registry.AuthConfig, error) {
	ctx, cancel := context.WithTimeout(parent, ecrAuthTimeout)
	defer cancel()

	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}
	client := ecr.NewFromConfig(awsCfg)
	out, err := client.GetAuthorizationToken(ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return nil, err
	}
	if len(out.AuthorizationData) == 0 || out.AuthorizationData[0].AuthorizationToken == nil {
		return nil, errors.New("ecr returned no authorization data")
	}

	raw, err := base64.StdEncoding.DecodeString(*out.AuthorizationData[0].AuthorizationToken)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return nil, errors.New("ecr auth token malformed")
	}
	return &registry.AuthConfig{
		Username:      parts[0],
		Password:      parts[1],
		ServerAddress: host,
	}, nil
}

// loadDockerConfigAuth is the original pre-Phase-4 resolver: look the
// host up in the user's Docker config.json, falling back to anonymous
// access when nothing matches. This is the code path used by Docker
// Hub, GHCR, GCR, and any generic private registry — essentially
// every registry that follows the standard Docker login flow.
func loadDockerConfigAuth(configPath, registryHost string) (*registry.AuthConfig, error) {
	path, err := resolveDockerConfigPath(configPath)
	if err != nil {
		return nil, nil //nolint:nilerr // missing home dir means anonymous, not fatal
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read docker config: %w", err)
	}

	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse docker config: %w", err)
	}

	auth, ok := lookupAuth(cfg.Auths, registryHost)
	if !ok {
		return nil, nil
	}

	username := auth.Username
	password := auth.Password
	if auth.Auth != "" && (username == "" || password == "") {
		if u, p, ok := decodeBasicAuth(auth.Auth); ok {
			username = u
			password = p
		}
	}

	if username == "" && password == "" {
		return nil, nil
	}

	return &registry.AuthConfig{
		Username:      username,
		Password:      password,
		ServerAddress: registryHost,
	}, nil
}

// resolveDockerConfigPath returns the absolute path to config.json. An
// explicit override (from config.Config.DockerConfig) takes precedence; it may
// be either the directory containing config.json or the file itself, matching
// the semantics of the DOCKER_CONFIG env var that Docker's own CLI uses.
func resolveDockerConfigPath(override string) (string, error) {
	if override != "" {
		if strings.HasSuffix(override, "config.json") {
			return override, nil
		}
		return filepath.Join(override, "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".docker", "config.json"), nil
}

// lookupAuth scans the auths map using the tolerant matching Docker itself
// uses: exact match, then with/without scheme, then the Docker Hub aliases,
// and finally URL-parsed host matching for keys of the form
// "https://<host>/path".
func lookupAuth(auths map[string]dockerConfigAuth, registryHost string) (dockerConfigAuth, bool) {
	candidates := []string{
		registryHost,
		"https://" + registryHost,
		"http://" + registryHost,
	}
	if registryHost == defaultRegistry {
		candidates = append(candidates,
			"https://index.docker.io/v1/",
			"https://index.docker.io/v2/",
			"index.docker.io",
			"docker.io",
		)
	}

	for _, key := range candidates {
		if v, ok := auths[key]; ok {
			return v, true
		}
	}

	// Fall back to host-level matching for keys like "https://ghcr.io/user".
	// We parse the key as a URL and compare host-to-host instead of using
	// strings.Contains — the latter would false-positive on lookalike hosts
	// such as "evil-ghcr.io.example" matching "ghcr.io".
	for key, v := range auths {
		if hostMatches(key, registryHost) {
			return v, true
		}
	}
	return dockerConfigAuth{}, false
}

func hostMatches(key, registryHost string) bool {
	candidate := key
	if !strings.Contains(candidate, "://") {
		candidate = "https://" + candidate
	}
	u, err := url.Parse(candidate)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, registryHost)
}

func decodeBasicAuth(encoded string) (string, string, bool) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}
