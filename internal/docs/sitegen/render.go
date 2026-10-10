package main

import (
	"bytes"
	"embed"
	"fmt"
	"html"
	"html/template"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

//go:embed templates.html
var templatesFS embed.FS

var tmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"inc": func(i int) int { return i + 1 },
}).ParseFS(templatesFS, "templates.html"))

type page struct {
	Slug        string
	Title       string
	Group       string
	Summary     template.HTML // inline HTML (may contain code)
	SummaryText string        // plain text, for meta descriptions
	Body        template.HTML
	TOC         []tocItem
	search      []searchEntry
}

type tocItem struct {
	ID, Title string
	Level     int
}

type navGroup struct {
	Title string
	Pages []*page
}

type indexData struct {
	Version string
	Groups  []navGroup
	Page    *page // always nil: the index is no topic
}

type topicData struct {
	Version    string
	Groups     []navGroup
	Page       *page
	Prev, Next *page
}

// searchEntry is one heading's section; short keys keep search.json small.
type searchEntry struct {
	Topic   string `json:"t"`
	Slug    string `json:"s"`
	Heading string `json:"h,omitempty"`
	Anchor  string `json:"a,omitempty"`
	Text    string `json:"x"`
}

// Long sections are cut in the search index: the start of a section is what matches
// and what the result snippet shows.
const maxSearchText = 1800

// renderTopic turns one topic into a page. The leading "# Title" and "*summary*" lines
// become the page header instead of being rendered in the body.
func renderTopic(slug string, src []byte, known map[string]bool) (*page, error) {
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(renderer.WithNodeRenderers(
			util.Prioritized(&codeRenderer{known: known, self: slug}, 100),
		)),
	)
	doc := md.Parser().Parse(text.NewReader(src))
	p := &page{Slug: slug, Title: slug}

	if h, ok := doc.FirstChild().(*ast.Heading); ok && h.Level == 1 {
		p.Title = plain(h, src)
		doc.RemoveChild(doc, h)
	}
	if para, ok := doc.FirstChild().(*ast.Paragraph); ok && para.ChildCount() == 1 {
		if em, ok := para.FirstChild().(*ast.Emphasis); ok {
			var b bytes.Buffer
			for c := em.FirstChild(); c != nil; c = c.NextSibling() {
				if err := md.Renderer().Render(&b, src, c); err != nil {
					return nil, err
				}
			}
			p.Summary = template.HTML(b.String())
			p.SummaryText = plain(em, src)
			doc.RemoveChild(doc, para)
		}
	}

	// Table of contents and search sections: split the body at h2/h3 headings.
	cur := &searchEntry{Topic: p.Title, Slug: slug}
	var body strings.Builder
	body.WriteString(p.SummaryText + " ") // the topic's own entry is never empty
	flush := func() {
		cur.Text = cut(strings.TrimSpace(body.String()), maxSearchText)
		if cur.Heading != "" || cur.Text != "" {
			p.search = append(p.search, *cur)
		}
		body.Reset()
	}
	for c := doc.FirstChild(); c != nil; c = c.NextSibling() {
		if h, ok := c.(*ast.Heading); ok && (h.Level == 2 || h.Level == 3) {
			flush()
			id := attr(h, "id")
			title := plain(h, src)
			p.TOC = append(p.TOC, tocItem{ID: id, Title: title, Level: h.Level})
			cur = &searchEntry{Topic: p.Title, Slug: slug, Heading: title, Anchor: id}
			continue
		}
		body.WriteString(plain(c, src))
		body.WriteByte(' ')
	}
	flush()

	var b bytes.Buffer
	if err := md.Renderer().Render(&b, src, doc); err != nil {
		return nil, err
	}
	p.Body = template.HTML(b.String())
	return p, nil
}

func attr(n ast.Node, name string) string {
	v, ok := n.AttributeString(name)
	if !ok {
		return ""
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprint(v)
}

// plain returns a node's text with whitespace collapsed.
func plain(n ast.Node, src []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		switch n := n.(type) {
		case *ast.Text:
			if entering {
				b.Write(n.Segment.Value(src))
				if n.SoftLineBreak() || n.HardLineBreak() {
					b.WriteByte(' ')
				}
			}
		case *ast.String:
			if entering {
				b.Write(n.Value)
			}
		case *ast.FencedCodeBlock, *ast.CodeBlock:
			if entering {
				lines := n.Lines()
				for i := 0; i < lines.Len(); i++ {
					seg := lines.At(i)
					b.Write(seg.Value(src))
				}
				b.WriteByte(' ')
			}
			return ast.WalkSkipChildren, nil
		default:
			if !entering && n.Type() == ast.TypeBlock {
				b.WriteByte(' ')
			}
		}
		return ast.WalkContinue, nil
	})
	return strings.Join(strings.Fields(b.String()), " ")
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	if i := strings.LastIndexByte(s, ' '); i > n/2 {
		s = s[:i]
	}
	return s + " …"
}

// ───────── Code ─────────

// codeRenderer renders code blocks as terminal-style panels with a copy button and light
// syntax colors, and turns inline `dokwalt docs <topic>` into links to that page.
type codeRenderer struct {
	known map[string]bool
	self  string
}

func (r *codeRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindFencedCodeBlock, r.block)
	reg.Register(ast.KindCodeBlock, r.block)
	reg.Register(ast.KindCodeSpan, r.span)
}

var docsRef = regexp.MustCompile(`^dokwalt docs ([a-z0-9-]+)$`)

func (r *codeRenderer) span(w util.BufWriter, src []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	var raw strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if t, ok := c.(*ast.Text); ok {
			raw.Write(t.Segment.Value(src))
		}
	}
	s := strings.ReplaceAll(raw.String(), "\n", " ")
	code := "<code>" + html.EscapeString(s) + "</code>"
	if m := docsRef.FindStringSubmatch(s); m != nil && r.known[m[1]] && m[1] != r.self {
		code = `<a class="xref" href="/docs/` + m[1] + `/">` + code + "</a>"
	}
	_, _ = w.WriteString(code)
	return ast.WalkSkipChildren, nil
}

const copyButton = `<button class="copy" type="button" aria-label="Copy"><svg class="i-copy" viewBox="0 0 24 24" aria-hidden="true"><rect x="9" y="9" width="12" height="12" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg><svg class="i-check" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 12.5l4.5 4.5L19 7.5"/></svg></button>`

var langLabel = map[string]string{"bash": "shell", "sh": "shell", "yaml": "yaml", "yml": "yaml", "ini": "ini", "json": "json"}

func (r *codeRenderer) block(w util.BufWriter, src []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	lang := ""
	if f, ok := n.(*ast.FencedCodeBlock); ok {
		lang = string(f.Language(src))
	}
	var lines []string
	l := n.Lines()
	for i := 0; i < l.Len(); i++ {
		seg := l.At(i)
		lines = append(lines, strings.TrimRight(string(seg.Value(src)), "\n"))
	}
	label := langLabel[lang]
	if label == "" {
		label = "output"
	}
	fmt.Fprintf(w, `<div class="code" data-lang="%s"><div class="code-bar"><span>%s</span>%s</div><pre><code>%s</code></pre></div>`+"\n",
		html.EscapeString(label), html.EscapeString(label), copyButton, highlight(lang, lines))
	return ast.WalkSkipChildren, nil
}

func span(class, s string) string {
	if s == "" {
		return ""
	}
	return `<span class="` + class + `">` + html.EscapeString(s) + "</span>"
}

func highlight(lang string, lines []string) string {
	out := make([]string, len(lines))
	cont := false // the previous shell line ended with a backslash
	for i, l := range lines {
		switch lang {
		case "bash", "sh":
			out[i] = hiShell(l, !cont)
			cont = strings.HasSuffix(l, `\`)
		case "yaml", "yml":
			out[i] = hiYAML(l)
		case "ini":
			out[i] = hiINI(l)
		case "json":
			out[i] = hiJSON(l)
		default:
			out[i] = hiText(l)
		}
	}
	return strings.Join(out, "\n")
}

// hiShell colors commands, flags, strings and comments. cmd says whether the line starts
// in command position (not a continuation of the previous line).
func hiShell(l string, cmd bool) string {
	var b strings.Builder
	for i := 0; i < len(l); {
		c := l[i]
		switch {
		case c == ' ' || c == '\t':
			b.WriteByte(c)
			i++
		case c == '#' && (i == 0 || l[i-1] == ' ' || l[i-1] == '\t'):
			b.WriteString(span("t-com", l[i:]))
			i = len(l)
		case c == '"' || c == '\'':
			j := strings.IndexByte(l[i+1:], c)
			end := len(l)
			if j >= 0 {
				end = i + 1 + j + 1
			}
			b.WriteString(span("t-str", l[i:end]))
			i, cmd = end, false
		case c == '|' || c == ';' || c == '&':
			b.WriteString(html.EscapeString(string(c)))
			i++
			cmd = true
		default:
			j := i
			for j < len(l) && !strings.ContainsRune(" \t|;&", rune(l[j])) {
				if q := l[j]; q == '"' || q == '\'' { // KEY="a b": the quotes belong to the word
					if k := strings.IndexByte(l[j+1:], q); k >= 0 {
						j += k + 1
					}
				}
				j++
			}
			word := l[i:j]
			switch {
			case cmd && !strings.Contains(word, "="):
				b.WriteString(span("t-cmd", word))
				cmd = word == "sudo" // `sudo dokwalt …`: the next word is the command too
			case strings.HasPrefix(word, "-"):
				b.WriteString(span("t-flag", word))
			default:
				b.WriteString(html.EscapeString(word))
				if !strings.Contains(word, "=") {
					cmd = false
				}
			}
			i = j
		}
	}
	return b.String()
}

// commentStart finds a # that starts a comment (at the line start or after a space,
// outside quotes), or -1.
func commentStart(l string) int {
	var q byte
	for i := 0; i < len(l); i++ {
		switch c := l[i]; {
		case q != 0:
			if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '#' && (i == 0 || l[i-1] == ' ' || l[i-1] == '\t'):
			return i
		}
	}
	return -1
}

var quoted = regexp.MustCompile(`"[^"]*"|'[^']*'`)

func colorQuoted(s string) string {
	var b strings.Builder
	last := 0
	for _, m := range quoted.FindAllStringIndex(s, -1) {
		b.WriteString(html.EscapeString(s[last:m[0]]))
		b.WriteString(span("t-str", s[m[0]:m[1]]))
		last = m[1]
	}
	b.WriteString(html.EscapeString(s[last:]))
	return b.String()
}

var yamlKey = regexp.MustCompile(`^(\s*(?:-\s+)?)([^\s#:'"][^:#]*?|"[^"]*")(:)(\s|$)`)

func hiYAML(l string) string {
	code, com := l, ""
	if i := commentStart(l); i >= 0 {
		code, com = l[:i], l[i:]
	}
	var b strings.Builder
	if m := yamlKey.FindStringSubmatchIndex(code); m != nil {
		b.WriteString(html.EscapeString(code[:m[3]]))
		b.WriteString(span("t-key", code[m[4]:m[5]]))
		b.WriteString(html.EscapeString(code[m[6]:m[7]]))
		code = code[m[7]:]
	}
	b.WriteString(colorQuoted(code))
	b.WriteString(span("t-com", com))
	return b.String()
}

func hiINI(l string) string {
	t := strings.TrimSpace(l)
	switch {
	case strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";"):
		return span("t-com", l)
	case strings.HasPrefix(t, "["):
		return span("t-cmd", l)
	}
	if k, v, ok := strings.Cut(l, "="); ok {
		return span("t-key", k) + "=" + colorQuoted(v)
	}
	return html.EscapeString(l)
}

var jsonTok = regexp.MustCompile(`"(?:[^"\\]|\\.)*"(\s*:)?|\b(?:true|false|null)\b|-?\d+(?:\.\d+)?`)

func hiJSON(l string) string {
	var b strings.Builder
	last := 0
	for _, m := range jsonTok.FindAllStringSubmatchIndex(l, -1) {
		b.WriteString(html.EscapeString(l[last:m[0]]))
		tok := l[m[0]:m[1]]
		switch {
		case m[2] >= 0: // a key: the string before a colon
			b.WriteString(span("t-key", l[m[0]:m[2]]) + html.EscapeString(l[m[2]:m[1]]))
		case tok[0] == '"':
			b.WriteString(span("t-str", tok))
		default:
			b.WriteString(span("t-num", tok))
		}
		last = m[1]
	}
	b.WriteString(html.EscapeString(l[last:]))
	return b.String()
}

// hiText colors command output: a typed `$ command`, and the ✓ / ✗ / ◆ markers the CLI prints.
func hiText(l string) string {
	if rest, ok := strings.CutPrefix(l, "$ "); ok {
		return span("t-com", "$") + " " + hiShell(rest, true)
	}
	s := html.EscapeString(l)
	for mark, class := range map[string]string{"✓": "t-ok", "✗": "t-err", "◆": "t-cmd"} {
		s = strings.ReplaceAll(s, mark, `<span class="`+class+`">`+mark+"</span>")
	}
	return s
}
