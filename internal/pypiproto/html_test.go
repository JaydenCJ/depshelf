// Tests for the PEP 503 HTML side: the anchor scanner that reads upstream
// pages and the renderer pip consumes.
package pypiproto

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func baseURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestParseHTMLExtractsFilesFromRealShapedPage(t *testing.T) {
	page := `<!DOCTYPE html><html><head><title>Links for demo</title></head><body>
	<h1>Links for demo</h1>
	<a href="https://files.example.test/demo-1.0.0.tar.gz#sha256=aabbcc">demo-1.0.0.tar.gz</a><br/>
	<a href="https://files.example.test/demo-1.0.0-py3-none-any.whl#sha256=ddeeff" data-requires-python="&gt;=3.8">demo-1.0.0-py3-none-any.whl</a><br/>
	</body></html>`
	p, err := ParseHTML([]byte(page), baseURL(t, "https://index.example.test/simple/demo/"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 2 {
		t.Fatalf("got %d files", len(p.Files))
	}
	if p.Files[0].Hashes["sha256"] != "aabbcc" {
		t.Fatalf("fragment hash lost: %+v", p.Files[0])
	}
	if p.Files[1].RequiresPython != ">=3.8" {
		t.Fatalf("data-requires-python not entity-decoded: %q", p.Files[1].RequiresPython)
	}
	if strings.Contains(p.Files[0].URL, "#") {
		t.Fatal("fragment must be stripped from the stored URL")
	}
}

func TestParseHTMLResolvesRelativeHrefs(t *testing.T) {
	page := `<a href="../../packages/demo-1.0.0.tar.gz#sha256=aa">demo-1.0.0.tar.gz</a>`
	p, err := ParseHTML([]byte(page), baseURL(t, "https://index.example.test/simple/demo/"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if p.Files[0].URL != "https://index.example.test/packages/demo-1.0.0.tar.gz" {
		t.Fatalf("relative href resolved to %q", p.Files[0].URL)
	}
}

func TestParseHTMLScannerRobustness(t *testing.T) {
	// <abbr> must not trip the "<a" scanner; uppercase <A HREF> must parse;
	// anchors without href or text are skipped; entities decode; truncated
	// markup never panics.
	page := `<abbr title="x">y</abbr><A HREF="/d.whl">d.whl</A>
	<a name="top"></a><a href="/empty.whl"></a><a href="/x.whl">x&amp;y.whl</a>`
	p, err := ParseHTML([]byte(page), baseURL(t, "https://x.example.test/"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 2 {
		t.Fatalf("parsed %+v", p.Files)
	}
	if p.Files[0].Filename != "d.whl" || p.Files[1].Filename != "x&y.whl" {
		t.Fatalf("filenames: %q, %q", p.Files[0].Filename, p.Files[1].Filename)
	}
	for _, truncated := range []string{"<a", "<a href=", `<a href="unterminated`, `<a href="/x">no close`} {
		if _, err := ParseHTML([]byte(truncated), nil, "demo"); err != nil {
			t.Errorf("truncated page %q errored: %v", truncated, err)
		}
	}
}

func TestParseHTMLHandlesYankedVariants(t *testing.T) {
	page := `<a href="/a.whl" data-yanked>a.whl</a>
	<a href="/b.whl" data-yanked="broken metadata">b.whl</a>
	<a href="/c.whl">c.whl</a>`
	p, err := ParseHTML([]byte(page), baseURL(t, "https://x.example.test/"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if string(p.Files[0].Yanked) != "true" {
		t.Fatalf("bare data-yanked = %s", p.Files[0].Yanked)
	}
	var reason string
	if err := json.Unmarshal(p.Files[1].Yanked, &reason); err != nil || reason != "broken metadata" {
		t.Fatalf("yanked reason = %s", p.Files[1].Yanked)
	}
	if p.Files[2].Yanked != nil {
		t.Fatal("unyanked file gained a yanked marker")
	}
}

func TestRenderHTMLProducesPEP503PageWithFragments(t *testing.T) {
	p := NewProject("demo")
	p.Files = []File{{
		Filename:       "demo-1.0.0.tar.gz",
		URL:            "https://files.example.test/orig.tar.gz",
		Hashes:         map[string]string{"sha256": "aabbcc"},
		RequiresPython: ">=3.9",
	}}
	out := string(p.RenderHTML("http://127.0.0.1:8417"))
	if !strings.Contains(out, `href="http://127.0.0.1:8417/pypi/files/demo/demo-1.0.0.tar.gz#sha256=aabbcc"`) {
		t.Fatalf("href wrong:\n%s", out)
	}
	if !strings.Contains(out, `data-requires-python="&gt;=3.9"`) {
		t.Fatalf("requires-python not escaped:\n%s", out)
	}
	if !strings.Contains(out, "<title>Links for demo</title>") {
		t.Fatal("PEP 503 title missing")
	}
}

func TestRenderHTMLMarksYankedFiles(t *testing.T) {
	p := NewProject("demo")
	p.Files = []File{
		{Filename: "a.whl", Yanked: json.RawMessage(`true`)},
		{Filename: "b.whl", Yanked: json.RawMessage(`"bad build"`)},
		{Filename: "c.whl", Yanked: json.RawMessage(`false`)},
	}
	out := string(p.RenderHTML("http://127.0.0.1:8417"))
	if !strings.Contains(out, `data-yanked=""`) {
		t.Fatal("bare yanked marker missing")
	}
	if !strings.Contains(out, `data-yanked="bad build"`) {
		t.Fatal("yanked reason missing")
	}
	if strings.Count(out, "data-yanked") != 2 {
		t.Fatal("yanked:false must not render a marker")
	}
}

func TestRenderParseRoundTrip(t *testing.T) {
	// What depshelf serves must be parseable by depshelf itself — that is
	// exactly what happens when one shelf chains to another as upstream.
	p := NewProject("demo")
	p.Files = []File{{
		Filename:       "demo-1.0.0-py3-none-any.whl",
		URL:            "https://files.example.test/orig.whl",
		Hashes:         map[string]string{"sha256": "aabbcc"},
		RequiresPython: ">=3.8,<4",
	}}
	rendered := p.RenderHTML("http://127.0.0.1:8417")
	back, err := ParseHTML(rendered, baseURL(t, "http://127.0.0.1:8417/pypi/simple/demo/"), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Files) != 1 {
		t.Fatalf("round trip produced %d files", len(back.Files))
	}
	f := back.Files[0]
	if f.Filename != "demo-1.0.0-py3-none-any.whl" || f.Hashes["sha256"] != "aabbcc" || f.RequiresPython != ">=3.8,<4" {
		t.Fatalf("round trip lost fields: %+v", f)
	}
}

func TestRenderRootIndexBothFormats(t *testing.T) {
	html := string(RenderRootHTML([]string{"requests", "typing-extensions"}))
	if !strings.Contains(html, `<a href="/pypi/simple/requests/">requests</a>`) {
		t.Fatalf("root index missing entry:\n%s", html)
	}
	jsonOut, err := RenderRootJSON([]string{"requests"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(jsonOut), `"name": "requests"`) {
		t.Fatalf("root JSON missing entry:\n%s", jsonOut)
	}
	if !strings.Contains(string(jsonOut), `"api-version": "1.0"`) {
		t.Fatal("meta header missing")
	}
}
