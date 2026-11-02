// SPDX-License-Identifier: LicenseRef-Mesh-Sustainable-Use-License
// Copyright (C) 2026 Bright Interaction AB

package web

import (
	"strings"
	"testing"

	xhtml "golang.org/x/net/html"
)

// renderMDSafe must neutralise payloads that ride in on ingested note bodies: inline
// HTML (a raw <script>) is dropped and unsafe link schemes (javascript:) are not
// emitted as live hrefs, while legitimate prose survives. renderMD (trusted embedded
// docs) intentionally does NOT sanitise, so the two must differ on hostile input,
// proving the safe path is actually doing work.
func TestRenderMDSafeStripsHostileHTML(t *testing.T) {
	src := []byte("# Title\n\n<script>alert(1)</script>\n\n[click](javascript:alert(2))\n\nplain text\n")

	safe := renderMDSafe(src)
	if strings.Contains(safe, "<script>") {
		t.Errorf("renderMDSafe leaked a <script> tag:\n%s", safe)
	}
	if strings.Contains(strings.ToLower(safe), "javascript:") {
		t.Errorf("renderMDSafe emitted a javascript: link:\n%s", safe)
	}
	if !strings.Contains(safe, "plain text") {
		t.Errorf("renderMDSafe dropped legitimate content:\n%s", safe)
	}

	if !strings.Contains(renderMD(src), "<script>") {
		t.Error("renderMD unexpectedly stripped HTML; the trusted/untrusted split would be moot")
	}
}

func TestRenderMDSafeShortURLDestinations(t *testing.T) {
	for _, tc := range []struct{ name, destination string }{
		{"empty", ""},
		{"one-byte", "."},
		{"two-byte", ".."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if err := recover(); err != nil {
					t.Fatalf("renderMDSafe panicked for destination %q: %v", tc.destination, err)
				}
			}()
			const label = "Short destination label"
			out := renderMDSafe([]byte("[" + label + "](" + tc.destination + ")"))
			if !strings.Contains(out, label) {
				t.Errorf("renderMDSafe dropped the link label for destination %q: %s", tc.destination, out)
			}
		})
	}
}

// liveTags is what renderMDSafe is allowed to emit. Anything outside this set in the
// parsed output means a note body managed to create an element, which is the whole
// attack: the SPA assigns this HTML to innerHTML.
var liveTags = map[string]bool{
	"html": true, "head": true, "body": true, // implied by the parser, never in the string
	"p": true, "a": true, "em": true, "strong": true, "del": true, "code": true, "pre": true,
	"blockquote": true, "hr": true, "br": true, "img": true,
	"ul": true, "ol": true, "li": true, "dl": true, "dt": true, "dd": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"table": true, "thead": true, "tbody": true, "tr": true, "th": true, "td": true,
	"sup": true, "sub": true, "span": true, "div": true,
}

// scanRendered parses rendered HTML and reports every element name and attribute name
// that ended up LIVE (as markup), as opposed to escaped text. Escaped text is safe by
// construction, so a substring check on the raw string would false-positive on payload
// echoes like &lt;svg/onload=...&gt;; parsing is the assertion that actually matches
// what a browser would do with innerHTML.
func scanRendered(t *testing.T, rendered string) (tags []string, attrs []string) {
	t.Helper()
	doc, err := xhtml.Parse(strings.NewReader(rendered))
	if err != nil {
		t.Fatalf("parse rendered html: %v", err)
	}
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			tags = append(tags, n.Data)
			for _, a := range n.Attr {
				attrs = append(attrs, strings.ToLower(a.Key))
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return tags, attrs
}

// TestRenderMDSafeBlocksAttributeInjection pins the two gomarkdown paths that write
// markdown-controlled text straight into an HTML attribute with no escaping: the
// fenced-code info string (class="language-...") and a {#custom-id} heading id. Before
// the sanitiser + the HeadingIDs removal these produced literally
//
//	<pre><code class="language-lang"><img/src=x/onerror=alert(1)>">code</code></pre>
//	<h1 id="x"><svg/onload=alert(2)>">Rel</h1>
//	<h1 id="y" onmouseover="alert(3)">B</h1>
//
// which is stored XSS the moment the SPA assigns it to innerHTML. The assertion is on
// the PARSED output: no element outside the markdown set, and no on* handler anywhere.
func TestRenderMDSafeBlocksAttributeInjection(t *testing.T) {
	cases := []struct {
		name    string
		src     string
		keeps   string // prose that must survive the sanitiser
		noLive  []string
		noAttrs []string
	}{
		{
			name:    "fenced code info string closes the class attribute",
			src:     "```lang\"><img/src=x/onerror=alert(1)>\nharmless code\n```\n",
			keeps:   "harmless code",
			noLive:  []string{"img"},
			noAttrs: []string{"onerror"},
		},
		{
			name:    "heading custom id closes the id attribute",
			src:     "# Rel {#x\"><svg/onload=alert(2)>}\n",
			keeps:   "Rel",
			noLive:  []string{"svg"},
			noAttrs: []string{"onload"},
		},
		{
			name:    "heading custom id appends an event handler",
			src:     "# B {#y\" onmouseover=\"alert(3)}\n",
			keeps:   "B",
			noAttrs: []string{"onmouseover"},
		},
		{
			name:    "fenced code info string appends an event handler",
			src:     "```js\"onmouseover=\"alert(4)\nx\n```\n",
			keeps:   "x",
			noAttrs: []string{"onmouseover"},
		},
		{
			name:   "image title attribute breakout",
			src:    "![a](/i.png \"t\\\"><img src=x onerror=alert(5)>\")\n",
			noLive: []string{"svg", "script"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := renderMDSafe([]byte(tc.src))
			tags, attrs := scanRendered(t, out)
			for _, tag := range tags {
				if !liveTags[tag] {
					t.Errorf("renderMDSafe emitted a live <%s> element:\n%s", tag, out)
				}
			}
			for _, a := range attrs {
				if strings.HasPrefix(a, "on") {
					t.Errorf("renderMDSafe emitted an event handler attribute %q:\n%s", a, out)
				}
			}
			for _, banned := range tc.noLive {
				for _, tag := range tags {
					if tag == banned {
						t.Errorf("renderMDSafe emitted a live <%s>:\n%s", banned, out)
					}
				}
			}
			for _, banned := range tc.noAttrs {
				for _, a := range attrs {
					if a == banned {
						t.Errorf("renderMDSafe emitted attribute %q:\n%s", banned, out)
					}
				}
			}
			if tc.keeps != "" && !strings.Contains(out, tc.keeps) {
				t.Errorf("renderMDSafe dropped legitimate content %q:\n%s", tc.keeps, out)
			}
		})
	}
}

// A clean fenced block keeps its language class, so the sanitiser did not cost us the
// only styling hook the docs/search panels rely on.
func TestRenderMDSafeKeepsCleanLanguageClass(t *testing.T) {
	out := renderMDSafe([]byte("```go\nfmt.Println()\n```\n"))
	if !strings.Contains(out, `class="language-go"`) {
		t.Errorf("clean fenced block lost its language class:\n%s", out)
	}
}
