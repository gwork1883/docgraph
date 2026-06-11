package localdocs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type Document struct {
	Path     string
	Title    string
	Hash     string
	Sections []Section
}

type Section struct {
	HeadingPath []string
	Title       string
	Content     string
	Ordinal     int
	Hash        string
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
		if !isMarkdownFile(entry.Name()) {
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
	doc := parseMarkdown(relPath, string(content))
	doc.Hash = hashBytes(content)
	return doc
}

func shouldSkipDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "node_modules", "vendor", "build", "dist", "out", "target", "coverage", "__pycache__":
		return true
	default:
		return false
	}
}

func isMarkdownFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown":
		return true
	default:
		return false
	}
}

func parseMarkdown(relPath, content string) Document {
	lines := strings.SplitAfter(content, "\n")
	title := filepath.Base(relPath)
	sections := make([]Section, 0)
	headingStack := make([]string, 0, 6)
	currentLines := make([]string, 0)
	currentTitle := ""
	currentPath := []string(nil)
	currentIsHeading := false
	sawHeading := false

	flush := func() {
		body := strings.Join(currentLines, "")
		if !currentIsHeading && strings.TrimSpace(body) == "" {
			currentLines = currentLines[:0]
			return
		}
		if !sawHeading {
			currentTitle = title
			currentPath = nil
		}
		sections = append(sections, Section{
			HeadingPath: cloneStrings(currentPath),
			Title:       currentTitle,
			Content:     body,
			Ordinal:     len(sections),
			Hash:        hashString(body),
		})
		currentLines = currentLines[:0]
	}

	for _, line := range lines {
		if line == "" {
			continue
		}
		if heading, ok := parseATXHeading(line); ok {
			flush()
			if heading.Level == 1 && heading.Title != "" && title == filepath.Base(relPath) {
				title = heading.Title
			}
			sawHeading = true
			headingStack = updateHeadingStack(headingStack, heading.Level, heading.Title)
			currentTitle = heading.Title
			currentPath = cloneStrings(headingStack)
			currentIsHeading = true
			continue
		}
		currentLines = append(currentLines, line)
	}
	flush()

	return Document{
		Path:     relPath,
		Title:    title,
		Sections: sections,
	}
}

type heading struct {
	Level int
	Title string
}

func parseATXHeading(line string) (heading, bool) {
	trimmed := strings.TrimRight(line, "\r\n")
	leadingSpaces := len(trimmed) - len(strings.TrimLeft(trimmed, " "))
	if leadingSpaces > 3 {
		return heading{}, false
	}

	s := trimmed[leadingSpaces:]
	level := 0
	for level < len(s) && level < 6 && s[level] == '#' {
		level++
	}
	if level == 0 {
		return heading{}, false
	}
	if level < len(s) && s[level] != ' ' && s[level] != '\t' {
		return heading{}, false
	}

	title := strings.TrimSpace(s[level:])
	title = stripClosingHashes(title)
	return heading{Level: level, Title: title}, true
}

func stripClosingHashes(title string) string {
	if title == "" || title[len(title)-1] != '#' {
		return title
	}
	i := len(title) - 1
	for i >= 0 && title[i] == '#' {
		i--
	}
	if i >= 0 && (title[i] == ' ' || title[i] == '\t') {
		return strings.TrimSpace(title[:i])
	}
	return title
}

func updateHeadingStack(stack []string, level int, title string) []string {
	if level <= len(stack) {
		stack = stack[:level-1]
	}
	stack = append(stack, title)
	return stack
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	clone := make([]string, len(values))
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
