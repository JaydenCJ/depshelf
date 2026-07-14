// Package mirror implements the read-through cache policy on top of the
// store: serve fresh cache, fetch on miss, verify integrity before
// persisting, fall back to stale metadata when the upstream is down, and
// never touch the network in offline mode.
package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/JaydenCJ/depshelf/internal/integrity"
	"github.com/JaydenCJ/depshelf/internal/npmproto"
	"github.com/JaydenCJ/depshelf/internal/pypiproto"
	"github.com/JaydenCJ/depshelf/internal/store"
	"github.com/JaydenCJ/depshelf/internal/version"
)

// Defaults for the two public registries.
const (
	DefaultNpmUpstream  = "https://registry.npmjs.org"
	DefaultPypiUpstream = "https://pypi.org/simple"

	// maxMetadataBytes caps metadata documents; the largest real
	// packuments are tens of MB, artifacts are not capped (integrity
	// checks protect them instead).
	maxMetadataBytes = 64 << 20
)

// Source records where a response came from, surfaced to clients via the
// X-Depshelf-Source header and to the log.
type Source string

const (
	SourceCache    Source = "cache"
	SourceUpstream Source = "upstream"
	SourceStale    Source = "stale"
)

// ErrNotFound means the package/artifact does not exist upstream or, in
// offline mode, is not in the store.
var ErrNotFound = errors.New("not found")

// UpstreamError wraps a network or HTTP failure with no cached fallback.
type UpstreamError struct{ Err error }

func (e *UpstreamError) Error() string { return "upstream unavailable: " + e.Err.Error() }
func (e *UpstreamError) Unwrap() error { return e.Err }

// Options configures a Mirror. Zero-valued fields get sane defaults.
type Options struct {
	Store        *store.Store
	NpmUpstream  string
	PypiUpstream string
	Offline      bool
	MetadataTTL  time.Duration // how long cached metadata counts as fresh
	Client       *http.Client
	Logf         func(format string, args ...any)
}

// Mirror is the read-through engine shared by both protocol frontends.
type Mirror struct {
	store        *store.Store
	npmUpstream  string
	pypiUpstream string
	offline      bool
	ttl          time.Duration
	client       *http.Client
	logf         func(format string, args ...any)

	mu       sync.Mutex
	inflight map[string]*sync.Mutex // per-artifact download locks
}

// New builds a Mirror from Options.
func New(o Options) *Mirror {
	m := &Mirror{
		store:        o.Store,
		npmUpstream:  strings.TrimSuffix(o.NpmUpstream, "/"),
		pypiUpstream: strings.TrimSuffix(o.PypiUpstream, "/"),
		offline:      o.Offline,
		ttl:          o.MetadataTTL,
		client:       o.Client,
		logf:         o.Logf,
		inflight:     map[string]*sync.Mutex{},
	}
	if m.npmUpstream == "" {
		m.npmUpstream = DefaultNpmUpstream
	}
	if m.pypiUpstream == "" {
		m.pypiUpstream = DefaultPypiUpstream
	}
	if m.ttl <= 0 {
		m.ttl = 15 * time.Minute
	}
	if m.client == nil {
		m.client = &http.Client{Timeout: 60 * time.Second}
	}
	if m.logf == nil {
		m.logf = func(string, ...any) {}
	}
	return m
}

// Offline reports whether the mirror runs with the network disabled.
func (m *Mirror) Offline() bool { return m.offline }

// lock serializes downloads of the same artifact so concurrent installs
// hit upstream once. The map is bounded by the number of distinct
// artifacts requested during the process lifetime.
func (m *Mirror) lock(key string) func() {
	m.mu.Lock()
	l, ok := m.inflight[key]
	if !ok {
		l = &sync.Mutex{}
		m.inflight[key] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (m *Mirror) fetch(ctx context.Context, url, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "depshelf/"+version.Version)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	return m.client.Do(req)
}

// NpmPackument returns the raw upstream-form packument for name.
func (m *Mirror) NpmPackument(ctx context.Context, name string) ([]byte, Source, error) {
	cached, mtime, rerr := m.store.ReadMetadata("npm", name)
	have := rerr == nil
	if m.offline {
		if have {
			return cached, SourceCache, nil
		}
		return nil, "", fmt.Errorf("npm/%s: %w", name, ErrNotFound)
	}
	if have && time.Since(mtime) < m.ttl {
		return cached, SourceCache, nil
	}
	url := m.npmUpstream + "/" + npmproto.EscapeName(name)
	resp, err := m.fetch(ctx, url, "application/json")
	if err != nil {
		return m.staleOr(cached, have, "npm/"+name, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		data, err := readCapped(resp.Body)
		if err != nil {
			return m.staleOr(cached, have, "npm/"+name, err)
		}
		if !json.Valid(data) {
			return m.staleOr(cached, have, "npm/"+name, errors.New("upstream sent invalid JSON"))
		}
		if err := m.store.WriteMetadata("npm", name, data); err != nil {
			return nil, "", err
		}
		return data, SourceUpstream, nil
	case resp.StatusCode == http.StatusNotFound:
		// 404 is authoritative: the package does not exist (or was
		// unpublished), so stale metadata must not resurrect it.
		return nil, "", fmt.Errorf("npm/%s: %w", name, ErrNotFound)
	default:
		return m.staleOr(cached, have, "npm/"+name, fmt.Errorf("upstream status %s", resp.Status))
	}
}

// staleOr serves the stale cached copy when the upstream misbehaves — the
// whole point of a mirror on a flaky network — or surfaces the failure.
func (m *Mirror) staleOr(cached []byte, have bool, what string, cause error) ([]byte, Source, error) {
	if have {
		m.logf("%s: upstream unavailable (%v); serving stale metadata", what, cause)
		return cached, SourceStale, nil
	}
	return nil, "", &UpstreamError{Err: cause}
}

// NpmTarball ensures the tarball is on disk and returns its path.
func (m *Mirror) NpmTarball(ctx context.Context, name, file string) (string, Source, error) {
	path, ok, err := m.store.ArtifactPath("npm", name, file)
	if err != nil {
		return "", "", err
	}
	if ok {
		return path, SourceCache, nil
	}
	if m.offline {
		return "", "", fmt.Errorf("npm/%s/%s: %w", name, file, ErrNotFound)
	}
	unlock := m.lock("npm/" + name + "/" + file)
	defer unlock()
	if path, ok, err = m.store.ArtifactPath("npm", name, file); err != nil || ok {
		return path, SourceCache, err
	}
	raw, _, err := m.NpmPackument(ctx, name)
	if err != nil {
		return "", "", err
	}
	ref, err := npmproto.FindTarball(raw, file)
	if err != nil {
		return "", "", err
	}
	if ref == nil {
		// No version publishes this filename: refuse rather than proxy
		// arbitrary upstream paths.
		return "", "", fmt.Errorf("npm/%s/%s: %w", name, file, ErrNotFound)
	}
	path, err = m.download(ctx, "npm", name, file, ref.URL, ref.Want)
	if err != nil {
		return "", "", err
	}
	return path, SourceUpstream, nil
}

// PypiProject returns the parsed project index for a normalized name.
func (m *Mirror) PypiProject(ctx context.Context, name string) (*pypiproto.Project, Source, error) {
	cached, mtime, rerr := m.store.ReadMetadata("pypi", name)
	have := rerr == nil
	parse := func(data []byte, src Source) (*pypiproto.Project, Source, error) {
		p, err := pypiproto.ParseJSON(data)
		if err != nil {
			return nil, "", err
		}
		p.Name = name
		return p, src, nil
	}
	if m.offline {
		if have {
			return parse(cached, SourceCache)
		}
		return nil, "", fmt.Errorf("pypi/%s: %w", name, ErrNotFound)
	}
	if have && time.Since(mtime) < m.ttl {
		return parse(cached, SourceCache)
	}
	url := m.pypiUpstream + "/" + name + "/"
	resp, err := m.fetch(ctx, url, "application/vnd.pypi.simple.v1+json, text/html;q=0.5")
	if err != nil {
		data, src, serr := m.staleOr(cached, have, "pypi/"+name, err)
		if serr != nil {
			return nil, "", serr
		}
		return parse(data, src)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		data, err := readCapped(resp.Body)
		if err == nil {
			var p *pypiproto.Project
			p, err = parseUpstreamPypi(data, resp, name)
			if err == nil {
				stored, merr := p.MarshalStored()
				if merr != nil {
					return nil, "", merr
				}
				if werr := m.store.WriteMetadata("pypi", name, stored); werr != nil {
					return nil, "", werr
				}
				return p, SourceUpstream, nil
			}
		}
		data, src, serr := m.staleOr(cached, have, "pypi/"+name, err)
		if serr != nil {
			return nil, "", serr
		}
		return parse(data, src)
	case resp.StatusCode == http.StatusNotFound:
		return nil, "", fmt.Errorf("pypi/%s: %w", name, ErrNotFound)
	default:
		data, src, serr := m.staleOr(cached, have, "pypi/"+name, fmt.Errorf("upstream status %s", resp.Status))
		if serr != nil {
			return nil, "", serr
		}
		return parse(data, src)
	}
}

// parseUpstreamPypi decodes a project page in whichever format the
// upstream chose: PEP 691 JSON, or PEP 503 HTML with hrefs resolved
// against the final (post-redirect) request URL.
func parseUpstreamPypi(data []byte, resp *http.Response, name string) (*pypiproto.Project, error) {
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "json") {
		p, err := pypiproto.ParseJSON(data)
		if err != nil {
			return nil, err
		}
		p.Name = name
		return p, nil
	}
	return pypiproto.ParseHTML(data, resp.Request.URL, name)
}

// PypiFile ensures the distribution file is on disk and returns its path.
func (m *Mirror) PypiFile(ctx context.Context, name, file string) (string, Source, error) {
	path, ok, err := m.store.ArtifactPath("pypi", name, file)
	if err != nil {
		return "", "", err
	}
	if ok {
		return path, SourceCache, nil
	}
	if m.offline {
		return "", "", fmt.Errorf("pypi/%s/%s: %w", name, file, ErrNotFound)
	}
	unlock := m.lock("pypi/" + name + "/" + file)
	defer unlock()
	if path, ok, err = m.store.ArtifactPath("pypi", name, file); err != nil || ok {
		return path, SourceCache, err
	}
	p, _, err := m.PypiProject(ctx, name)
	if err != nil {
		return "", "", err
	}
	f := p.FindFile(file)
	if f == nil {
		return "", "", fmt.Errorf("pypi/%s/%s: %w", name, file, ErrNotFound)
	}
	var want integrity.Hash
	if sha := f.Hashes["sha256"]; sha != "" {
		want = integrity.Hash{Algo: "sha256", Hex: strings.ToLower(sha)}
	}
	path, err = m.download(ctx, "pypi", name, file, f.URL, want)
	if err != nil {
		return "", "", err
	}
	return path, SourceUpstream, nil
}

// download fetches url and persists it through the store's verifying
// atomic writer. A failed integrity check leaves nothing behind.
func (m *Mirror) download(ctx context.Context, eco, name, file, url string, want integrity.Hash) (string, error) {
	resp, err := m.fetch(ctx, url, "")
	if err != nil {
		return "", &UpstreamError{Err: err}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		// proceed
	case resp.StatusCode == http.StatusNotFound:
		return "", fmt.Errorf("%s/%s/%s: %w", eco, name, file, ErrNotFound)
	default:
		return "", &UpstreamError{Err: fmt.Errorf("upstream status %s for %s", resp.Status, url)}
	}
	path, sha, size, err := m.store.WriteArtifact(eco, name, file, resp.Body, want)
	if err != nil {
		return "", fmt.Errorf("storing %s/%s/%s: %w", eco, name, file, err)
	}
	m.logf("%s/%s: cached %s (%d bytes, sha256 %s…)", eco, name, file, size, sha[:12])
	return path, nil
}

func readCapped(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMetadataBytes {
		return nil, fmt.Errorf("metadata document exceeds %d bytes", maxMetadataBytes)
	}
	return data, nil
}
