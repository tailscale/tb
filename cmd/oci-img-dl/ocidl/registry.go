// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package ocidl

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Ref is a parsed image reference.
type Ref struct {
	Registry    string // "ghcr.io"
	Repo        string // "cirruslabs/macos-tahoe-xcode"
	TagOrDigest string // "26.5" or "sha256:..."
}

// ParseRef parses "[registry/]repo[:tag|@digest]". The registry defaults
// to ghcr.io and the tag to "latest".
func ParseRef(s string) (Ref, error) {
	var r Ref
	name := s
	if i := strings.Index(s, "@"); i >= 0 {
		name, r.TagOrDigest = s[:i], s[i+1:]
	} else if i := strings.LastIndex(s, ":"); i > strings.LastIndex(s, "/") {
		name, r.TagOrDigest = s[:i], s[i+1:]
	} else {
		r.TagOrDigest = "latest"
	}
	first, rest, ok := strings.Cut(name, "/")
	if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.Registry, r.Repo = first, rest
	} else {
		r.Registry, r.Repo = "ghcr.io", name
	}
	if r.Repo == "" || r.TagOrDigest == "" {
		return Ref{}, fmt.Errorf("bad image reference %q", s)
	}
	return r, nil
}

func (r Ref) String() string {
	sep := ":"
	if strings.HasPrefix(r.TagOrDigest, "sha256:") {
		sep = "@"
	}
	return r.Registry + "/" + r.Repo + sep + r.TagOrDigest
}

// client fetches from one repository, handling the registry's anonymous
// bearer token challenge.
type client struct {
	hc   *http.Client
	base string // "https://ghcr.io/v2/cirruslabs/macos-tahoe-xcode"

	mu    sync.Mutex
	token string
}

// scheme is the URL scheme for registries. Tests set it to "http".
var scheme = "https"

func newClient(r Ref) *client {
	return &client{hc: http.DefaultClient, base: scheme + "://" + r.Registry + "/v2/" + r.Repo}
}

// get fetches path under the repository, authenticating if challenged.
// The caller must close the response body.
func (c *client) get(ctx context.Context, path, accept string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "GET", c.base+path, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		c.mu.Lock()
		tok := c.token
		c.mu.Unlock()
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		res, err := c.hc.Do(req)
		if err != nil {
			return nil, err
		}
		if res.StatusCode == http.StatusUnauthorized && attempt == 0 {
			challenge := res.Header.Get("WWW-Authenticate")
			res.Body.Close()
			if err := c.authenticate(ctx, challenge); err != nil {
				return nil, err
			}
			continue
		}
		if res.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<10))
			res.Body.Close()
			return nil, fmt.Errorf("GET %s: %s: %s", c.base+path, res.Status, strings.TrimSpace(string(b)))
		}
		return res, nil
	}
}

// authenticate gets an anonymous token for a
// `Bearer realm="...",service="...",scope="..."` challenge.
func (c *client) authenticate(ctx context.Context, challenge string) error {
	scheme, params, _ := strings.Cut(challenge, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return fmt.Errorf("unsupported auth challenge %q", challenge)
	}
	p := map[string]string{}
	for _, kv := range strings.Split(params, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(kv), "=")
		p[k] = strings.Trim(v, `"`)
	}
	if p["realm"] == "" {
		return fmt.Errorf("auth challenge %q has no realm", challenge)
	}
	q := url.Values{}
	for _, k := range []string{"service", "scope"} {
		if p[k] != "" {
			q.Set(k, p[k])
		}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", p["realm"]+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("getting token: %s", res.Status)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&t); err != nil {
		return fmt.Errorf("getting token: %w", err)
	}
	tok := cmp.Or(t.Token, t.AccessToken)
	if tok == "" {
		return errors.New("token response has no token")
	}
	c.mu.Lock()
	c.token = tok
	c.mu.Unlock()
	return nil
}

// descriptor is an OCI content descriptor.
type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// manifest is an OCI image manifest.
type manifest struct {
	MediaType   string            `json:"mediaType"`
	Config      descriptor        `json:"config"`
	Layers      []descriptor      `json:"layers"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

const mediaTypeManifest = "application/vnd.oci.image.manifest.v1+json"

// manifest fetches the manifest for r, returning it and its raw bytes.
func (c *client) manifest(ctx context.Context, r Ref) (*manifest, []byte, error) {
	res, err := c.get(ctx, "/manifests/"+r.TagOrDigest, mediaTypeManifest)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return nil, nil, err
	}
	m := new(manifest)
	if err := json.Unmarshal(raw, m); err != nil {
		return nil, nil, fmt.Errorf("parsing manifest: %w", err)
	}
	if m.MediaType != "" && m.MediaType != mediaTypeManifest {
		return nil, nil, fmt.Errorf("unsupported manifest media type %q", m.MediaType)
	}
	return m, raw, nil
}

// blob opens the blob with the given digest.
func (c *client) blob(ctx context.Context, digest string) (io.ReadCloser, error) {
	res, err := c.get(ctx, "/blobs/"+digest, "")
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}
