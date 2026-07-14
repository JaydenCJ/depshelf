// Package server exposes the mirror over HTTP: an npm-registry-compatible
// surface under /npm/ and a PEP 503 / PEP 691 simple index under /pypi/.
// Routing is done by hand so scoped npm names ("@scope/name", encoded or
// not) and the "/-/" tarball separator are handled precisely.
package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/JaydenCJ/depshelf/internal/allowlist"
	"github.com/JaydenCJ/depshelf/internal/mirror"
	"github.com/JaydenCJ/depshelf/internal/npmproto"
	"github.com/JaydenCJ/depshelf/internal/pypiproto"
	"github.com/JaydenCJ/depshelf/internal/store"
	"github.com/JaydenCJ/depshelf/internal/version"
)

const pypiJSONType = "application/vnd.pypi.simple.v1+json"

// Config wires the server's collaborators together.
type Config struct {
	Mirror *mirror.Mirror
	Store  *store.Store
	Allow  *allowlist.List // nil = allow everything
	// PublicURL overrides the base URL written into rewritten links;
	// empty derives "http://<request Host>" per request.
	PublicURL string
	Logf      func(format string, args ...any)
}

// Server implements http.Handler.
type Server struct {
	cfg Config
}

// New returns the ready-to-serve handler.
func New(cfg Config) *Server {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Server{cfg: cfg}
}

// statusWriter records the response code for the access log.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// ServeHTTP logs every request and dispatches to the two protocol trees.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	start := time.Now()
	s.route(sw, r)
	s.cfg.Logf("%s %s -> %d (%s, %s)", r.Method, r.URL.Path, sw.status,
		sw.Header().Get("X-Depshelf-Source"), time.Since(start).Round(time.Millisecond))
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/":
		s.handleStatus(w, r)
	case p == "/healthz":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	case p == "/npm" || strings.HasPrefix(p, "/npm/"):
		s.handleNpm(w, r)
	case p == "/pypi" || strings.HasPrefix(p, "/pypi/"):
		s.handlePypi(w, r)
	default:
		s.jsonError(w, http.StatusNotFound, "not found")
	}
}

// baseURL is the origin written into rewritten download links.
func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.PublicURL != "" {
		return strings.TrimSuffix(s.cfg.PublicURL, "/")
	}
	return "http://" + r.Host
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	mode := "read-through"
	if s.cfg.Mirror.Offline() {
		mode = "offline"
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"name":"depshelf","version":%q,"mode":%q,"npm":"/npm/","pypi":"/pypi/simple/"}`+"\n",
		version.Version, mode)
}

func (s *Server) jsonError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, "{\"error\": %q}\n", msg)
}

// writeMirrorError maps mirror-layer failures to HTTP statuses.
func (s *Server) writeMirrorError(w http.ResponseWriter, err error) {
	var ue *mirror.UpstreamError
	switch {
	case errors.Is(err, mirror.ErrNotFound):
		s.jsonError(w, http.StatusNotFound, err.Error())
	case errors.As(err, &ue):
		s.jsonError(w, http.StatusBadGateway, err.Error())
	default:
		s.jsonError(w, http.StatusInternalServerError, err.Error())
	}
}

// ---- npm ----------------------------------------------------------------

func (s *Server) handleNpm(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/npm"), "/")
	if strings.HasPrefix(rest, "-/") {
		s.handleNpmMeta(w, r, rest)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if rest == "" {
		s.jsonError(w, http.StatusNotFound, "specify a package name, e.g. /npm/left-pad")
		return
	}
	if i := strings.Index(rest, "/-/"); i >= 0 {
		s.handleNpmTarball(w, r, rest[:i], rest[i+3:])
		return
	}
	s.handleNpmPackument(w, r, rest)
}

// handleNpmMeta answers the registry side-channel endpoints npm CLI calls
// during install; empty documents keep audit quiet without lying about
// advisories the mirror cannot know.
func (s *Server) handleNpmMeta(w http.ResponseWriter, r *http.Request, rest string) {
	switch rest {
	case "-/npm/v1/security/advisories/bulk", "-/npm/v1/security/audits/quick":
		if r.Method != http.MethodPost {
			s.jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, "{}")
	case "-/ping":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, "{}")
	default:
		s.jsonError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) checkNpmAccess(w http.ResponseWriter, name string) bool {
	if err := npmproto.ValidateName(name); err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return false
	}
	if !s.cfg.Allow.Allowed("npm", name) {
		s.jsonError(w, http.StatusForbidden, "package not in allowlist: npm:"+name)
		return false
	}
	return true
}

func (s *Server) handleNpmPackument(w http.ResponseWriter, r *http.Request, name string) {
	if !s.checkNpmAccess(w, name) {
		return
	}
	raw, src, err := s.cfg.Mirror.NpmPackument(r.Context(), name)
	if err != nil {
		s.writeMirrorError(w, err)
		return
	}
	body, err := npmproto.Rewrite(raw, s.baseURL(r), name)
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Depshelf-Source", string(src))
	w.Write(body)
}

func (s *Server) handleNpmTarball(w http.ResponseWriter, r *http.Request, name, file string) {
	if !s.checkNpmAccess(w, name) {
		return
	}
	if err := store.ValidateFilename(file); err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	path, src, err := s.cfg.Mirror.NpmTarball(r.Context(), name, file)
	if err != nil {
		s.writeMirrorError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Depshelf-Source", string(src))
	http.ServeFile(w, r, path)
}

// ---- PyPI ---------------------------------------------------------------

func (s *Server) handlePypi(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/pypi"), "/")
	switch {
	case rest == "" || rest == "simple":
		http.Redirect(w, r, "/pypi/simple/", http.StatusMovedPermanently)
	case rest == "simple/":
		s.handlePypiRoot(w, r)
	case strings.HasPrefix(rest, "simple/"):
		s.handlePypiProject(w, r, strings.TrimPrefix(rest, "simple/"))
	case strings.HasPrefix(rest, "files/"):
		s.handlePypiFile(w, r, strings.TrimPrefix(rest, "files/"))
	default:
		s.jsonError(w, http.StatusNotFound, "not found")
	}
}

// wantsPEP691 checks whether the client negotiated the JSON simple API.
func wantsPEP691(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), pypiJSONType)
}

// handlePypiRoot lists the locally cached projects. The mirror does not
// proxy the full multi-megabyte upstream index; pip only needs per-project
// pages for installs, and airgapped browsing should show what is on shelf.
func (s *Server) handlePypiRoot(w http.ResponseWriter, r *http.Request) {
	pkgs, err := s.cfg.Store.List()
	if err != nil {
		s.jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var names []string
	for _, p := range pkgs {
		if p.Ecosystem == "pypi" {
			names = append(names, p.Name)
		}
	}
	if wantsPEP691(r) {
		body, err := pypiproto.RenderRootJSON(names)
		if err != nil {
			s.jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", pypiJSONType)
		w.Write(body)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(pypiproto.RenderRootHTML(names))
}

func (s *Server) handlePypiProject(w http.ResponseWriter, r *http.Request, rest string) {
	proj := strings.TrimSuffix(rest, "/")
	hadSlash := strings.HasSuffix(rest, "/")
	if proj == "" || strings.Contains(proj, "/") {
		s.jsonError(w, http.StatusNotFound, "not found")
		return
	}
	// PEP 503: non-normalized lookups redirect to the canonical URL.
	if norm := pypiproto.Normalize(proj); norm != proj || !hadSlash {
		http.Redirect(w, r, "/pypi/simple/"+norm+"/", http.StatusMovedPermanently)
		return
	}
	if !pypiproto.ValidNormalized(proj) {
		s.jsonError(w, http.StatusBadRequest, "invalid project name")
		return
	}
	if !s.cfg.Allow.Allowed("pypi", proj) {
		s.jsonError(w, http.StatusForbidden, "package not in allowlist: pypi:"+proj)
		return
	}
	p, src, err := s.cfg.Mirror.PypiProject(r.Context(), proj)
	if err != nil {
		s.writeMirrorError(w, err)
		return
	}
	w.Header().Set("X-Depshelf-Source", string(src))
	if wantsPEP691(r) {
		body, err := p.RenderJSON(s.baseURL(r))
		if err != nil {
			s.jsonError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", pypiJSONType)
		w.Write(body)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(p.RenderHTML(s.baseURL(r)))
}

func (s *Server) handlePypiFile(w http.ResponseWriter, r *http.Request, rest string) {
	proj, file, ok := strings.Cut(rest, "/")
	if !ok || proj == "" || file == "" || strings.Contains(file, "/") {
		s.jsonError(w, http.StatusNotFound, "not found")
		return
	}
	if !pypiproto.ValidNormalized(proj) {
		s.jsonError(w, http.StatusBadRequest, "invalid project name")
		return
	}
	if err := store.ValidateFilename(file); err != nil {
		s.jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.cfg.Allow.Allowed("pypi", proj) {
		s.jsonError(w, http.StatusForbidden, "package not in allowlist: pypi:"+proj)
		return
	}
	path, src, err := s.cfg.Mirror.PypiFile(r.Context(), proj, file)
	if err != nil {
		s.writeMirrorError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Depshelf-Source", string(src))
	http.ServeFile(w, r, path)
}
