package clean_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/motherlodelab/magpie/clean"
)

var llmFixtures = []string{"article", "product", "spa-shell"}

func TestLLMText_Goldens(t *testing.T) {
	for _, name := range llmFixtures {
		got := cleanFile(t, name)
		goldenDir(t, "llm", name+".llm.md", clean.ToLLMText(got))
		goldenDir(t, "llm", name+".txt", clean.ToText(got.Markdown))
	}
}

// Reduction is the US-1 contract (bytes; len/4 token proxy per capTokens).
// It binds on content-scale pages: on sub-1KB shells the metadata header +
// gated structured data necessarily outweigh the body, and shrinking further
// would mean dropping content. Small fixtures are covered by
// LinksPreserved + StructuredGate + NoAddedMarkers instead.
func TestLLMText_Reduction(t *testing.T) {
	got := cleanFile(t, "article")
	llm := clean.ToLLMText(got)
	if len(llm) >= len(got.Markdown) {
		t.Errorf("article: len(llm)=%d >= len(md)=%d, want shrink", len(llm), len(got.Markdown))
	}
}

func TestLLMText_NoAddedMarkers(t *testing.T) {
	for _, name := range llmFixtures {
		got := cleanFile(t, name)
		llm := clean.ToLLMText(got)
		body := llm
		for _, h := range []string{"\n## Links", "\n## Structured Data"} {
			if i := strings.Index(body, h); i >= 0 {
				body = body[:i]
			}
		}
		if strings.Contains(body, "**") || strings.Contains(body, "`") || strings.Contains(body, "![") {
			t.Errorf("%s: body section kept markdown markers:\n%s", name, body)
		}
	}
}

// Anchors only (mirrors extractLinks): image targets ![alt](src) are excluded.
var mdTargetRe = regexp.MustCompile(`[^!]\[([^\]]*)\]\((https?://[^)\s]+)\)`)

func TestLLMText_LinksPreserved(t *testing.T) {
	for _, name := range llmFixtures {
		got := cleanFile(t, name)
		llm := clean.ToLLMText(got)
		idx := strings.Index(llm, "## Links")
		if idx < 0 {
			if mdTargetRe.FindString(got.Markdown) != "" {
				t.Errorf("%s: markdown has links but llm output has no ## Links section", name)
			}
			continue
		}
		footer := llm[idx:]
		for _, m := range mdTargetRe.FindAllStringSubmatch(got.Markdown, -1) {
			if !strings.Contains(footer, m[2]) {
				t.Errorf("%s: link target %q missing from ## Links", name, m[2])
			}
		}
	}
}

// TestMarkdownLinks pins the exported link walk the watch metrics use:
// anchors only (images and #fragments skipped), deduped, document order.
func TestMarkdownLinks(t *testing.T) {
	md := "[a](https://x/1) ![i](https://x/img.png) [b](#top) [c](https://x/1) [d](https://x/2)"
	got := clean.MarkdownLinks(md)
	if want := []string{"https://x/1", "https://x/2"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("MarkdownLinks = %q, want %q", got, want)
	}
	if got := clean.MarkdownLinks(""); len(got) != 0 {
		t.Errorf("MarkdownLinks(\"\") = %q, want empty", got)
	}
}

func TestLLMText_StructuredGate(t *testing.T) {
	big := strings.Repeat("x", 600)
	side := `{"@type":"Article","name":"T","articleBody":"` + big + `","nested":{"@type":"WebPage","url":"https://example.com/"},"site":{"@type":"WebSite","name":"Example"}}`
	p := clean.CleanedPage{
		Title: "T", Markdown: "# T\n\n" + strings.Repeat("prose ", 50),
		StructuredData: json.RawMessage(side),
	}
	out := clean.ToLLMText(p)
	if strings.Contains(out, "WebSite") || strings.Contains(out, `"@type": "WebPage"`) || strings.Contains(out, "WebPage") {
		t.Errorf("structured gate kept chrome blocks:\n%s", out)
	}
	if strings.Contains(out, big[:100]) {
		t.Error("structured gate kept articleBody dupe")
	}
	if i := strings.Index(out, "## Structured Data"); i >= 0 {
		if len(out[i:]) > 16*1024+512 {
			t.Errorf("structured block exceeds 16KB cap: %d", len(out[i:]))
		}
	}
}

func TestLLMText_TopLevelChromeDropped(t *testing.T) {
	p := clean.CleanedPage{
		Title: "T", Markdown: "# T\n\n" + strings.Repeat("prose ", 50),
		StructuredData: json.RawMessage(`{"@context":"https://schema.org","@type":"WebSite","name":"Example","url":"https://example.com/"}`),
	}
	out := clean.ToLLMText(p)
	if strings.Contains(out, "WebSite") || strings.Contains(out, "Structured Data") {
		t.Errorf("top-level WebSite sidecar leaked into output:\n%s", out)
	}
}

func TestToText_Table(t *testing.T) {
	cases := map[string]string{
		"# Hello":                "Hello",
		"**bold** and *it* x":    "bold and it x",
		"`code` here":            "code here",
		"[text](http://ex.com/)": "text",
		"> quoted":               "quoted",
	}
	for in, want := range cases {
		if got := clean.ToText(in); got != want {
			t.Errorf("ToText(%q) = %q, want %q", in, got, want)
		}
	}
	if got := clean.ToText("![alt](http://ex.com/i.png)\n\nkeep"); got != "keep" {
		t.Errorf("ToText image drop = %q, want %q", got, "keep")
	}
}

func TestRender_Matrix(t *testing.T) {
	got := cleanFile(t, "article")
	if out, err := clean.Render(got, ""); err != nil || out != got.Markdown {
		t.Errorf("Render(\"\") identity failed: %v", err)
	}
	if out, err := clean.Render(got, "markdown"); err != nil || out != got.Markdown {
		t.Errorf("Render(markdown) identity failed: %v", err)
	}
	if _, err := clean.Render(got, "bogus"); err == nil ||
		!strings.Contains(err.Error(), "markdown|llm|text|json|html") {
		t.Errorf("Render(bogus) = %v, want valid-values error", err)
	}
	// Phase G: html returns the cleaned (scope-applied) document verbatim.
	if out, err := clean.Render(got, "html"); err != nil || out != got.HTML {
		t.Errorf("Render(html) = %d bytes, err %v; want the CleanedPage.HTML verbatim", len(out), err)
	}
	if got.HTML == "" {
		t.Error("article page must carry a non-empty HTML field")
	}
	// PDF pages have no HTML: markdown rides in the <article> wrapper.
	pdf := clean.CleanedPage{Markdown: "extracted pdf text"}
	wrapper, err := clean.Render(pdf, "html")
	if err != nil {
		t.Fatal(err)
	}
	if want := "<article>\nextracted pdf text\n</article>\n"; wrapper != want {
		t.Errorf("PDF html wrapper = %q, want %q", wrapper, want)
	}
	out, err := clean.Render(got, "json")
	if err != nil {
		t.Fatalf("Render(json): %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("render json invalid: %v", err)
	}
	for _, k := range []string{"url", "final_url", "title", "markdown", "structured_data",
		"content", "metadata", "links", "word_count"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("render json missing key %q", k)
		}
	}
}
