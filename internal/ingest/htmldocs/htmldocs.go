package htmldocs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/net/html"
)

type Document struct {
	Path     string
	Title    string
	URL      string
	Hash     string
	Anchors  []string
	Sections []Section
}

type Section struct {
	HeadingPath []string
	Title       string
	Anchor      string
	Content     string
	Ordinal     int
	Hash        string
	Links       []Link
}

type Link struct {
	Href string
	Text string
}

func Scan(root string) ([]Document, error) {
	var docs []Document
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && shouldSkipDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isHTMLFile(entry.Name()) {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relative path for %s: %w", path, err)
		}
		doc := Parse(filepath.ToSlash(rel), content)
		docs = append(docs, doc)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return docs, nil
}

func Parse(relPath string, content []byte) Document {
	root, err := html.Parse(strings.NewReader(string(content)))
	if err != nil {
		return Document{
			Path:  relPath,
			Title: filepath.Base(relPath),
			Hash:  hashBytes(content),
		}
	}
	title := strings.TrimSpace(firstText(root, "title"))
	if title == "" {
		title = filepath.Base(relPath)
	}

	parser := pageParser{
		docTitle: title,
		sections: make([]Section, 0),
	}
	parser.walk(root)
	parser.flush()
	if len(parser.sections) == 0 {
		parser.sections = append(parser.sections, Section{
			Title:       title,
			HeadingPath: []string{title},
			Content:     strings.TrimSpace(textContent(root)),
			Ordinal:     0,
		})
	}
	for i := range parser.sections {
		parser.sections[i].Ordinal = i
		parser.sections[i].Hash = hashString(parser.sections[i].Content)
	}

	return Document{
		Path:     relPath,
		Title:    title,
		Hash:     hashBytes(content),
		Anchors:  uniqueStrings(parser.anchors),
		Sections: parser.sections,
	}
}

type pageParser struct {
	docTitle      string
	headingPath   []string
	currentPath   []string
	currentAnchor string
	currentTitle  string
	currentLines  []string
	currentLinks  []Link
	anchors       []string
	sections      []Section
}

func (p *pageParser) walk(node *html.Node) {
	if node.Type == html.ElementNode {
		if id := strings.TrimSpace(attr(node, "id")); id != "" {
			p.anchors = append(p.anchors, id)
		}
		switch node.Data {
		case "script", "style", "noscript", "nav":
			return
		case "a":
			href := strings.TrimSpace(attr(node, "href"))
			if isInternalHTMLLink(href) {
				p.currentLinks = append(p.currentLinks, Link{Href: href, Text: strings.TrimSpace(textContent(node))})
			}
		case "h1", "h2", "h3", "h4", "h5", "h6":
			p.flush()
			level := int(node.Data[1] - '0')
			title := strings.TrimSpace(textContent(node))
			if title == "" {
				title = p.docTitle
			}
			if level <= len(p.headingPath) {
				p.headingPath = p.headingPath[:level-1]
			}
			p.headingPath = append(p.headingPath, title)
			p.currentTitle = title
			p.currentPath = cloneStrings(p.headingPath)
			p.currentAnchor = strings.TrimSpace(attr(node, "id"))
			return
		case "p", "li", "pre", "code":
			text := strings.TrimSpace(textContent(node))
			if text != "" {
				p.currentLines = append(p.currentLines, text)
			}
			p.currentLinks = append(p.currentLinks, internalLinks(node)...)
			return
		case "div", "span":
			// SPA pages often put visible text inside <div> or <span> instead
			// of traditional <p>/<li>. Only extract text from leaf-level div/span
			// that contain direct text and no block-level child elements.
			if isLeafTextElement(node) {
				text := strings.TrimSpace(textContent(node))
				if text != "" {
					p.currentLines = append(p.currentLines, text)
				}
				p.currentLinks = append(p.currentLinks, internalLinks(node)...)
				return
			}
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		p.walk(child)
	}
}

func (p *pageParser) flush() {
	content := strings.TrimSpace(strings.Join(p.currentLines, "\n"))
	if content == "" && p.currentTitle == "" {
		return
	}
	title := p.currentTitle
	path := p.currentPath
	if title == "" {
		title = p.docTitle
		path = []string{p.docTitle}
	}
	p.sections = append(p.sections, Section{
		Title:       title,
		HeadingPath: cloneStrings(path),
		Anchor:      p.currentAnchor,
		Content:     content,
		Links:       cloneLinks(p.currentLinks),
	})
	p.currentLines = nil
	p.currentLinks = nil
}

func firstText(node *html.Node, tag string) string {
	if node.Type == html.ElementNode && node.Data == tag {
		return textContent(node)
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if text := firstText(child, tag); text != "" {
			return text
		}
	}
	return ""
}

func textContent(node *html.Node) string {
	var parts []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			text := strings.TrimSpace(n.Data)
			if text != "" {
				parts = append(parts, text)
			}
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return strings.Join(parts, " ")
}

// isLeafTextElement checks if a <div> or <span> is a "leaf" element that
// contains direct text and no block-level children. We want to extract text
// from these elements (common in SPA-rendered pages) but skip container
// divs that just wrap other elements (like <div id="app">).
func isLeafTextElement(node *html.Node) bool {
	hasDirectText := false
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.TextNode {
			if strings.TrimSpace(child.Data) != "" {
				hasDirectText = true
			}
		} else if child.Type == html.ElementNode {
			switch child.Data {
			case "div", "section", "article", "main", "header", "footer",
				"ul", "ol", "dl", "table", "form", "fieldset",
				"h1", "h2", "h3", "h4", "h5", "h6", "p", "blockquote",
				"pre", "hr", "iframe":
				// Contains a block-level child → this is a container, not a leaf
				return false
			}
		}
	}
	return hasDirectText
}

func attr(node *html.Node, key string) string {
	for _, attr := range node.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func internalLinks(node *html.Node) []Link {
	links := make([]Link, 0)
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			href := strings.TrimSpace(attr(n, "href"))
			if isInternalHTMLLink(href) {
				links = append(links, Link{Href: href, Text: strings.TrimSpace(textContent(n))})
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(node)
	return links
}

func isInternalHTMLLink(href string) bool {
	href = strings.TrimSpace(href)
	if href == "" {
		return false
	}
	lower := strings.ToLower(href)
	if strings.Contains(lower, "://") || strings.HasPrefix(lower, "mailto:") || strings.HasPrefix(lower, "tel:") || strings.HasPrefix(lower, "javascript:") || strings.HasPrefix(lower, "data:") {
		return false
	}
	target := strings.SplitN(strings.SplitN(href, "?", 2)[0], "#", 2)[0]
	if target == "" {
		return true
	}
	if strings.HasSuffix(target, "/") {
		return true
	}
	switch strings.ToLower(filepath.Ext(target)) {
	case ".html", ".htm":
		return true
	default:
		return !strings.Contains(filepath.Base(target), ".")
	}
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func shouldSkipDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "dist" || name == "build"
}

func isHTMLFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".html", ".htm":
		return true
	default:
		return false
	}
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	clone := make([]string, len(values))
	copy(clone, values)
	return clone
}

func cloneLinks(values []Link) []Link {
	if len(values) == 0 {
		return nil
	}
	clone := make([]Link, len(values))
	copy(clone, values)
	return clone
}

func hashString(value string) string {
	return hashBytes([]byte(value))
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
