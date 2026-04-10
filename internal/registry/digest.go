package registry

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/docker/docker/api/types/registry"
)

const (
	mediaTypeManifestV2     = "application/vnd.docker.distribution.manifest.v2+json"
	mediaTypeManifestListV2 = "application/vnd.docker.distribution.manifest.list.v2+json"
	mediaTypeOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeOCIIndex       = "application/vnd.oci.image.index.v1+json"
)

var acceptHeader = strings.Join([]string{
	mediaTypeManifestListV2,
	mediaTypeManifestV2,
	mediaTypeOCIIndex,
	mediaTypeOCIManifest,
}, ", ")

var httpClient = &http.Client{Timeout: 30 * time.Second}

// platformDescriptor is the part of a manifest-list entry we actually care
// about when picking the per-platform manifest.
type platformDescriptor struct {
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType"`
	Platform  struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
		Variant      string `json:"variant,omitempty"`
	} `json:"platform"`
}

type manifestList struct {
	Manifests []platformDescriptor `json:"manifests"`
}

// manifest is the subset of a v2 / OCI image manifest we parse after
// resolving to a single platform. All we need is the image config descriptor
// so we can compare its SHA256 against the local image ID.
type manifest struct {
	Config struct {
		Digest string `json:"digest"`
	} `json:"config"`
}

// FetchRemoteIdentifiers contacts the registry HTTP API v2 and returns every
// sha256 identifier that might legitimately describe the remote image from
// some Docker client's perspective. The watcher's comparator treats the image
// as up-to-date if any local identifier intersects any remote identifier; see
// docker.LocalImageIdentifiers for why the identity of "the" image digest has
// become version-dependent.
//
// The flow is:
//
//  1. GET /v2/{repo}/manifests/{tag} with an Accept header that covers both
//     manifest and index media types.
//  2. Record the top-level Docker-Content-Digest header. For single-manifest
//     repos this is the manifest digest; for multi-arch repos it is the index
//     digest. In both cases this is what modern containerd-based image stores
//     report as the local .Id and inside .RepoDigests.
//  3. If the response is a manifest list / OCI index, pick the leaf manifest
//     for linux/<runtime.GOARCH> (falling back to linux/amd64), GET it, and
//     also record its digest — that is what pre-containerd Docker Engines
//     store in .RepoDigests after resolving a multi-arch pull.
//  4. Record the leaf manifest's .config.digest as well. That is what
//     traditional Docker Engines expose as the image config blob SHA via .Id.
//
// Deduped return order is: top manifest digest, leaf manifest digest, leaf
// config digest. Any empty value is skipped.
func FetchRemoteIdentifiers(ref *Reference, auth *registry.AuthConfig) ([]string, error) {
	reference := ref.Tag
	if reference == "" && ref.Digest != "" {
		reference = ref.Digest
	}
	if reference == "" {
		return nil, fmt.Errorf("no tag or digest to look up")
	}

	seen := make(map[string]struct{}, 3)
	var ids []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	topDigest, body, contentType, err := getManifest(ref, reference, auth)
	if err != nil {
		return nil, err
	}
	add(topDigest)

	if isManifestList(contentType) {
		platformDigest, err := pickPlatformManifest(body)
		if err != nil {
			return nil, fmt.Errorf("pick platform manifest: %w", err)
		}
		add(platformDigest)
		leafDigestHeader, leafBody, _, err := getManifest(ref, platformDigest, auth)
		if err != nil {
			return nil, fmt.Errorf("resolve platform manifest: %w", err)
		}
		add(leafDigestHeader)
		body = leafBody
	}

	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	add(m.Config.Digest)

	if len(ids) == 0 {
		return nil, fmt.Errorf("registry returned no identifiers for %s", reference)
	}
	return ids, nil
}

func isManifestList(contentType string) bool {
	ct := strings.ToLower(contentType)
	return strings.Contains(ct, "manifest.list") || strings.Contains(ct, "image.index")
}

// getManifest issues a GET against /v2/{repo}/manifests/{reference}. If the
// registry replies with a 401 + Www-Authenticate: Bearer, we fetch a token and
// retry once. Returns (Docker-Content-Digest, body, Content-Type).
func getManifest(ref *Reference, reference string, auth *registry.AuthConfig) (string, []byte, string, error) {
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", ref.Registry, ref.Repository, reference)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", nil, "", fmt.Errorf("build manifest request: %w", err)
	}
	req.Header.Set("Accept", acceptHeader)
	if auth != nil && auth.Username != "" {
		req.SetBasicAuth(auth.Username, auth.Password)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", nil, "", fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("Www-Authenticate")
		if strings.HasPrefix(strings.ToLower(challenge), "bearer") {
			token, err := fetchBearerToken(challenge, auth)
			if err != nil {
				return "", nil, "", fmt.Errorf("fetch bearer token: %w", err)
			}
			req2, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				return "", nil, "", err
			}
			req2.Header.Set("Accept", acceptHeader)
			req2.Header.Set("Authorization", "Bearer "+token)
			resp2, err := httpClient.Do(req2)
			if err != nil {
				return "", nil, "", fmt.Errorf("GET %s (with token): %w", url, err)
			}
			defer resp2.Body.Close()
			return readManifest(resp2, url)
		}
	}

	return readManifest(resp, url)
}

func readManifest(resp *http.Response, url string) (string, []byte, string, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, "", fmt.Errorf("read manifest body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", nil, "", fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp.Header.Get("Docker-Content-Digest"), body, resp.Header.Get("Content-Type"), nil
}

// fetchBearerToken parses a Www-Authenticate: Bearer challenge and requests a
// token from the realm. Supports the anonymous case (no basic auth).
func fetchBearerToken(challenge string, auth *registry.AuthConfig) (string, error) {
	params := parseChallenge(challenge)
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("bearer challenge missing realm")
	}

	req, err := http.NewRequest(http.MethodGet, realm, nil)
	if err != nil {
		return "", err
	}
	q := req.URL.Query()
	if svc, ok := params["service"]; ok {
		q.Set("service", svc)
	}
	if scope, ok := params["scope"]; ok {
		q.Set("scope", scope)
	}
	req.URL.RawQuery = q.Encode()

	if auth != nil && auth.Username != "" {
		req.SetBasicAuth(auth.Username, auth.Password)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Body may echo back the credentials in some registries — do NOT
		// include it in the error. Status code alone is enough to diagnose.
		return "", fmt.Errorf("token endpoint returned status %d", resp.StatusCode)
	}

	var tokenResp struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", err
	}
	if tokenResp.Token != "" {
		return tokenResp.Token, nil
	}
	return tokenResp.AccessToken, nil
}

// parseChallenge extracts the key="value" pairs from a Www-Authenticate
// bearer challenge header. Not a full parser, but enough for the registry.
func parseChallenge(challenge string) map[string]string {
	params := map[string]string{}
	rest := strings.TrimSpace(challenge)
	if idx := strings.IndexAny(rest, " \t"); idx >= 0 {
		rest = rest[idx+1:]
	}
	for _, pair := range splitOutsideQuotes(rest, ',') {
		pair = strings.TrimSpace(pair)
		eq := strings.Index(pair, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(pair[:eq])
		value := strings.TrimSpace(pair[eq+1:])
		value = strings.Trim(value, `"`)
		params[key] = value
	}
	return params
}

func splitOutsideQuotes(s string, sep rune) []string {
	var out []string
	var b strings.Builder
	inQuotes := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			b.WriteRune(r)
		case r == sep && !inQuotes:
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// pickPlatformManifest walks a manifest list and picks the entry for the
// current runtime.GOARCH under linux. Defaults to linux/amd64 on mismatches.
func pickPlatformManifest(body []byte) (string, error) {
	var list manifestList
	if err := json.Unmarshal(body, &list); err != nil {
		return "", err
	}
	if len(list.Manifests) == 0 {
		return "", fmt.Errorf("manifest list has no entries")
	}

	wantOS := "linux"
	wantArch := runtime.GOARCH
	if wantArch == "" {
		wantArch = "amd64"
	}

	for _, m := range list.Manifests {
		if m.Platform.OS == wantOS && m.Platform.Architecture == wantArch {
			return m.Digest, nil
		}
	}
	// Fall back to linux/amd64 if our arch isn't in the list.
	for _, m := range list.Manifests {
		if m.Platform.OS == "linux" && m.Platform.Architecture == "amd64" {
			return m.Digest, nil
		}
	}
	return list.Manifests[0].Digest, nil
}
