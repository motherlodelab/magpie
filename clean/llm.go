package clean

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// llmStructuredCap caps the ## Structured Data block (16 KB).
const llmStructuredCap = 16 * 1024

// ToLLMText renders a token-optimized plain-text view: metadata header →
// cleaned body → deduped ## Links → gated ## Structured Data.
func ToLLMText(p CleanedPage) string {
	var b strings.Builder
	if h := llmHeader(p); h != "" {
		b.WriteString(h)
		b.WriteString("\n\n")
	}
	body, links := llmBody(p.Markdown)
	b.WriteString(strings.TrimSpace(body))
	if len(links) > 0 {
		b.WriteString("\n\n## Links\n\n")
		for _, l := range links {
			fmt.Fprintf(&b, "- [%s](%s)\n", l.text, l.url)
		}
	}
	if sd := llmStructured(p.StructuredData, body); sd != "" {
		b.WriteString("\n## Structured Data\n\n")
		b.WriteString(sd)
	}
	return strings.TrimSpace(b.String()) + "\n"
}

func llmHeader(p CleanedPage) string {
	if p.Title == "" && p.Metadata.Description == "" && p.Metadata.Author == "" &&
		p.Metadata.Date == "" && p.FinalURL == "" {
		return ""
	}
	var b strings.Builder
	title := p.Title
	if title == "" {
		title = "Untitled"
	}
	b.WriteString("# " + title + "\n")
	if p.FinalURL != "" {
		b.WriteString("> Source: " + p.FinalURL + "\n")
	}
	if p.Metadata.Description != "" {
		b.WriteString("> " + oneLine(p.Metadata.Description) + "\n")
	}
	var byline []string
	if p.Metadata.Author != "" {
		byline = append(byline, "By "+p.Metadata.Author)
	}
	if p.Metadata.Date != "" {
		byline = append(byline, p.Metadata.Date)
	}
	if len(byline) > 0 {
		b.WriteString("> " + strings.Join(byline, " · ") + "\n")
	}
	return strings.TrimSpace(b.String())
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

type llmLink struct {
	text, url string
}

// extractLinks is the single walk over markdown link targets, split into
// anchors and image sources. llmBody footers the anchors, renderJSON reports
// both, and every consumer shares the dedupe and the ![alt](src) guard.
func extractLinks(md string) (anchors, images []llmLink) {
	seenA, seenI := map[string]bool{}, map[string]bool{}
	for _, idx := range mdLinkRe.FindAllStringSubmatchIndex(md, -1) {
		text, u := md[idx[2]:idx[3]], md[idx[4]:idx[5]]
		if u == "" || strings.HasPrefix(u, "#") {
			continue
		}
		if strings.TrimSpace(text) == "" {
			text = u
		} else {
			text = strings.TrimSpace(text)
		}
		if idx[0] > 0 && md[idx[0]-1] == '!' {
			if !seenI[u] {
				seenI[u] = true
				images = append(images, llmLink{text: text, url: u})
			}
			continue
		}
		if !seenA[u] {
			seenA[u] = true
			anchors = append(anchors, llmLink{text: text, url: u})
		}
	}
	return anchors, images
}

func linkURLs(ls []llmLink) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.url)
	}
	return out
}

var (
	mdLinkRe     = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	mdImageRe    = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)
	boldRe       = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	italicRe     = regexp.MustCompile(`(^|[\s(])\*([^*\n]+)\*([\s).,;:!?]|$)`)
	codeRe       = regexp.MustCompile("`([^`\n]+)`")
	cssClassRe   = regexp.MustCompile(`^\s*(\.[a-zA-Z][\w-]*)(\s+\.[a-zA-Z][\w-]*)+\s*$`)
	cssBlobRe    = regexp.MustCompile(`^\s*\{[^}]{20,}\}\s*$`)
	paginateRe   = regexp.MustCompile(`(?i)(/page/\d+|#comments|#reply|reply-to|comment-\d+)`)
	blockquoteRe = regexp.MustCompile(`^\s*>\s?`)
	tableSepRe   = regexp.MustCompile(`^\s*\|(?:[\s:\-]*\|)+\s*$`)
	statNumRe    = regexp.MustCompile(`^\s*[\d.,]+%?\s*$`)
	statLabelRe  = regexp.MustCompile(`^\s*[A-Za-z][\w\s&/-]{1,40}\s*$`)
	headingRe    = regexp.MustCompile(`(?m)^#{1,6}\s+`)
	quoteRe      = regexp.MustCompile(`(?m)^\s*>\s?`)
	fenceRe      = regexp.MustCompile("`{3}[^`]*`{3}")
	spacesRe     = regexp.MustCompile(`[ \t]+`)
	lineLeadRe   = regexp.MustCompile(`(?m)^[ \t]+`)
	multiBlankRe = regexp.MustCompile(`\n{3,}`)
)

// llmBody cleans markdown line-by-line and extracts deduped links.
func llmBody(md string) (string, []llmLink) {
	lines := strings.Split(md, "\n")
	var out []string
	seenPara := map[string]bool{}
	anchors, _ := extractLinks(md)
	var links []llmLink
	for _, l := range anchors {
		if paginateRe.MatchString(l.url) || strings.Contains(strings.ToLower(l.text), "reply") {
			continue
		}
		links = append(links, l)
	}
	for _, ln := range lines {
		// Drop decorative images (empty alt); keep informative ones as alt text.
		if m := mdImageRe.FindStringSubmatch(ln); m != nil {
			if strings.TrimSpace(m[1]) == "" {
				continue
			}
			ln = mdImageRe.ReplaceAllString(ln, "$1")
		}
		// Links become plain text in the body (targets live in ## Links).
		ln = mdLinkRe.ReplaceAllString(ln, "$1")
		// Strip bold/italic/code markers, keep text.
		ln = boldRe.ReplaceAllString(ln, "$1")
		ln = italicRe.ReplaceAllString(ln, "${1}$2$3")
		ln = codeRe.ReplaceAllString(ln, "$1")
		ln = blockquoteRe.ReplaceAllString(ln, "")
		ln = strings.TrimSpace(ln)
		// GFM tables: drop the zero-content separator row, compact padding.
		if strings.HasPrefix(ln, "|") {
			if strings.Contains(ln, "-") && tableSepRe.MatchString(ln) {
				continue
			}
			ln = compactTableRow(ln)
		}
		if ln == "" {
			out = append(out, "")
			continue
		}
		// Drop CSS-class noise lines and {…} blobs.
		if cssClassRe.MatchString(ln) || cssBlobRe.MatchString(ln) {
			continue
		}
		// Drop pagination/comment links.
		if paginateRe.MatchString(ln) {
			continue
		}
		out = append(out, ln)
	}
	out = dropLogoRuns(out)
	out = mergeStatLines(out)
	// Dedup paragraphs/headings preserving order.
	var deduped []string
	for _, ln := range out {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "|") || strings.HasPrefix(t, "- ") || strings.HasPrefix(t, ">") {
			if t != "" && strings.HasPrefix(t, "#") {
				if seenPara["h:"+t] {
					continue
				}
				seenPara["h:"+t] = true
			}
			deduped = append(deduped, ln)
			continue
		}
		if seenPara["p:"+t] {
			continue
		}
		seenPara["p:"+t] = true
		deduped = append(deduped, ln)
	}
	// Collapse 3+ blank runs to one.
	var final []string
	blanks := 0
	for _, ln := range deduped {
		if strings.TrimSpace(ln) == "" {
			blanks++
			if blanks > 1 {
				continue
			}
		} else {
			blanks = 0
		}
		final = append(final, ln)
	}
	return strings.TrimSpace(strings.Join(final, "\n")), links
}

// compactTableRow trims per-cell padding: "| a  | b |" → "| a | b |".
func compactTableRow(ln string) string {
	cells := strings.Split(ln, "|")
	for i, c := range cells {
		cells[i] = strings.TrimSpace(c)
	}
	// Drop the empties from leading/trailing pipes, keep inner empties
	// (colspan mirrors) so column counts survive.
	if len(cells) > 0 && cells[0] == "" {
		cells = cells[1:]
	}
	if len(cells) > 0 && cells[len(cells)-1] == "" {
		cells = cells[:len(cells)-1]
	}
	return "| " + strings.Join(cells, " | ") + " |"
}

func isShortLine(ln string) bool {
	t := strings.TrimSpace(ln)
	return t != "" && len(t) < 40 && !strings.ContainsAny(t, ".!?|:") &&
		!strings.HasPrefix(t, "#") && !strings.HasPrefix(t, "|") && !strings.HasPrefix(t, "- ") && !strings.HasPrefix(t, ">")
}

// dropLogoRuns removes runs of ≥2 consecutive short site-name/logo lines.
// Single short lines survive (they may be real headings); a run never is.
func dropLogoRuns(lines []string) []string {
	var out []string
	for i := 0; i < len(lines); {
		if isShortLine(lines[i]) {
			j := i
			for j < len(lines) && isShortLine(lines[j]) {
				j++
			}
			if j-i >= 2 {
				i = j
				continue
			}
		}
		out = append(out, lines[i])
		i++
	}
	return out
}

// mergeStatLines joins "12.99" + "Price" pairs into "Price: 12.99".
func mergeStatLines(lines []string) []string {
	var out []string
	for i := 0; i < len(lines); i++ {
		if i+1 < len(lines) && statNumRe.MatchString(lines[i]) && statLabelRe.MatchString(lines[i+1]) {
			out = append(out, strings.TrimSpace(lines[i+1])+": "+strings.TrimSpace(lines[i]))
			i++
			continue
		}
		out = append(out, lines[i])
	}
	return out
}

// llmStructured gates the sidecar: drops WebSite/WebPage chrome blocks,
// scrubs >500-char strings already contained in the body, caps at 16 KB.
func llmStructured(raw json.RawMessage, body string) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	v = scrubStructured(v, body)
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	s := string(b)
	if len(s) > llmStructuredCap {
		s = s[:llmStructuredCap] + "\n…(truncated)"
	}
	if strings.TrimSpace(s) == "null" || strings.TrimSpace(s) == "{}" || strings.TrimSpace(s) == "[]" {
		return ""
	}
	return "```json\n" + s + "\n```\n"
}

// scrubStructured drops WebSite/WebPage chrome blocks wholesale, skips
// articleBody dupes, and prunes long strings already contained in the body.
func scrubStructured(v any, body string) any {
	switch t := v.(type) {
	case map[string]any:
		if ty, _ := t["@type"].(string); ty == "WebSite" || ty == "WebPage" {
			return nil
		}
		out := map[string]any{}
		for k, val := range t {
			if k == "articleBody" {
				continue
			}
			sv := scrubStructured(val, body)
			if sv == nil {
				continue // dropped chrome block or pruned dupe
			}
			if s, ok := sv.(string); ok && len(s) > 500 && strings.Contains(body, s[:200]) {
				continue
			}
			out[k] = sv
		}
		return out
	case []any:
		var out []any
		for _, e := range t {
			out = append(out, scrubStructured(e, body))
		}
		if out == nil {
			return []any{}
		}
		return out
	case string:
		if len(t) > 500 && strings.Contains(body, t[:200]) {
			return ""
		}
		return t
	default:
		return v
	}
}

// ToText strips markdown markers to plain text (link→text, images dropped).
func ToText(md string) string {
	s := mdImageRe.ReplaceAllString(md, "")
	s = mdLinkRe.ReplaceAllString(s, "$1")
	s = headingRe.ReplaceAllString(s, "")
	s = boldRe.ReplaceAllString(s, "$1")
	s = italicRe.ReplaceAllString(s, "${1}$2$3")
	s = codeRe.ReplaceAllString(s, "$1")
	s = quoteRe.ReplaceAllString(s, "")
	s = fenceRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "`", "")
	s = strings.ReplaceAll(s, "*", "")
	s = spacesRe.ReplaceAllString(s, " ")
	s = lineLeadRe.ReplaceAllString(s, "")
	s = multiBlankRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// Render renders a CleanedPage in the named format: ""/markdown = identity,
// llm = ToLLMText, text = ToText, json = page envelope, html = the cleaned
// (scope-applied) document — PDFs have none, so their markdown rides in a
// minimal <article> wrapper instead.
func Render(p CleanedPage, format string) (string, error) {
	switch format {
	case "", "markdown":
		return p.Markdown, nil
	case "llm":
		return ToLLMText(p), nil
	case "text":
		return ToText(p.Markdown), nil
	case "json":
		return renderJSON(p), nil
	case "html":
		if p.HTML != "" {
			return p.HTML, nil
		}
		return "<article>\n" + p.Markdown + "\n</article>\n", nil
	default:
		return "", fmt.Errorf("clean: page format %q must be markdown|llm|text|json|html", format)
	}
}

func renderJSON(p CleanedPage) string {
	anchors, imgs := extractLinks(p.Markdown)
	links, images := linkURLs(anchors), linkURLs(imgs)
	if links == nil {
		links = []string{}
	}
	if images == nil {
		images = []string{}
	}
	env := map[string]any{
		"url":             p.FinalURL,
		"final_url":       p.FinalURL,
		"title":           p.Title,
		"markdown":        p.Markdown,
		"structured_data": jsonRaw(p.StructuredData),
		"page_format":     "json",
		"content":         p.Markdown,
		"metadata":        p.Metadata,
		"links":           links,
		"images":          images,
		"word_count":      WordCount(p.Markdown),
	}
	b, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}

func jsonRaw(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(r, &v); err != nil {
		return string(r)
	}
	return v
}
