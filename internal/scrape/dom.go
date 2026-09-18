package scrape

import (
	"bytes"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// parseHTML builds a document tree. The parser is the same one the Go
// toolchain ships for this purpose and never fails on malformed markup -- it
// recovers the way a browser does, which is the only useful behaviour when
// the input is someone else's production page.
func parseHTML(body []byte) (*html.Node, error) {
	return html.Parse(bytes.NewReader(body))
}

// walk visits every node depth-first. Returning false from fn skips that
// node's children, which is how the text extractors avoid descending into
// <script> and <style>.
func walk(n *html.Node, fn func(*html.Node) bool) {
	if n == nil {
		return
	}
	if !fn(n) {
		return
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		walk(child, fn)
	}
}

// attr returns an attribute's value, or "".
func attr(n *html.Node, key string) string {
	if n == nil {
		return ""
	}
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}

// hasClass reports whether a node carries a class, matched whole rather than
// as a substring: "a-price" must not match "a-price-range".
func hasClass(n *html.Node, class string) bool {
	for _, field := range strings.Fields(attr(n, "class")) {
		if field == class {
			return true
		}
	}
	return false
}

// classContains reports whether any of a node's classes contains a fragment.
// Needed where a site generates class names with a stable prefix and a
// volatile suffix, which is most of Flipkart's markup.
func classContains(n *html.Node, fragment string) bool {
	for _, field := range strings.Fields(attr(n, "class")) {
		if strings.Contains(field, fragment) {
			return true
		}
	}
	return false
}

// elementsByTag collects every element with the given tag name.
func elementsByTag(root *html.Node, tag string) []*html.Node {
	var found []*html.Node
	walk(root, func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == tag {
			found = append(found, n)
		}
		return true
	})
	return found
}

// elementByID returns the first element with the given id, or nil.
func elementByID(root *html.Node, id string) *html.Node {
	var found *html.Node
	walk(root, func(n *html.Node) bool {
		if found != nil {
			return false
		}
		if n.Type == html.ElementNode && attr(n, "id") == id {
			found = n
			return false
		}
		return true
	})
	return found
}

// elementsByClass collects every element carrying a class.
func elementsByClass(root *html.Node, class string) []*html.Node {
	var found []*html.Node
	walk(root, func(n *html.Node) bool {
		if n.Type == html.ElementNode && hasClass(n, class) {
			found = append(found, n)
		}
		return true
	})
	return found
}

// firstByClass returns the first element carrying a class, or nil.
func firstByClass(root *html.Node, class string) *html.Node {
	all := elementsByClass(root, class)
	if len(all) == 0 {
		return nil
	}
	return all[0]
}

// textOf returns the visible text of a subtree, whitespace collapsed.
//
// Script and style contents are skipped: a page's inline JavaScript is not
// text a shopper sees, and including it turns every extracted description
// into a wall of minified code.
func textOf(n *html.Node) string {
	if n == nil {
		return ""
	}
	var sb strings.Builder
	walk(n, func(node *html.Node) bool {
		if node.Type == html.ElementNode {
			switch node.Data {
			case "script", "style", "noscript", "template":
				return false
			case "br", "p", "li", "tr", "div":
				sb.WriteByte(' ')
			}
		}
		if node.Type == html.TextNode {
			sb.WriteString(node.Data)
		}
		return true
	})
	return collapseSpace(sb.String())
}

// scriptContents returns the raw text of every <script> element, optionally
// only those whose source contains a marker. Pages carry their real data in
// these far more often than in markup now.
func scriptContents(root *html.Node, marker string) []string {
	var out []string
	for _, script := range elementsByTag(root, "script") {
		var sb strings.Builder
		for child := script.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == html.TextNode {
				sb.WriteString(child.Data)
			}
		}
		text := sb.String()
		if marker == "" || strings.Contains(text, marker) {
			out = append(out, text)
		}
	}
	return out
}

// metaContent returns the content of the first <meta> matching a property or
// name attribute, e.g. metaContent(doc, "og:title").
func metaContent(root *html.Node, key string) string {
	for _, meta := range elementsByTag(root, "meta") {
		if strings.EqualFold(attr(meta, "property"), key) ||
			strings.EqualFold(attr(meta, "name"), key) ||
			strings.EqualFold(attr(meta, "itemprop"), key) {
			if content := strings.TrimSpace(attr(meta, "content")); content != "" {
				return content
			}
		}
	}
	return ""
}

// metaContents returns every matching meta content, for keys that legitimately
// repeat (og:image on a gallery page).
func metaContents(root *html.Node, key string) []string {
	var out []string
	for _, meta := range elementsByTag(root, "meta") {
		if strings.EqualFold(attr(meta, "property"), key) || strings.EqualFold(attr(meta, "name"), key) {
			if content := strings.TrimSpace(attr(meta, "content")); content != "" {
				out = append(out, content)
			}
		}
	}
	return out
}

// absolute resolves a possibly-relative reference against the page it was
// found on. Protocol-relative "//cdn.example/x.jpg" is common enough in
// retail markup to be worth handling explicitly.
func absolute(base *url.URL, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || base == nil {
		return ref
	}
	if strings.HasPrefix(ref, "//") {
		return base.Scheme + ":" + ref
	}
	parsed, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	return base.ResolveReference(parsed).String()
}
