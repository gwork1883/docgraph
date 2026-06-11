package staticdocs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/docgraph/docgraph/internal/ingest/htmldocs"
	"github.com/docgraph/docgraph/internal/ingest/localdocs"
)

type Config struct {
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

func Scan(root string, configJSON string) ([]htmldocs.Document, error) {
	cfg, err := parseConfig(configJSON)
	if err != nil {
		return nil, err
	}
	if err := validateRoot(root); err != nil {
		return nil, err
	}

	docs := make([]htmldocs.Document, 0)
	err = filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk %s: %w", filePath, walkErr)
		}
		rel, err := filepath.Rel(root, filePath)
		if err != nil {
			return fmt.Errorf("relative path for %s: %w", filePath, err)
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if filePath != root && shouldSkipDir(entry.Name(), rel, cfg) {
				return filepath.SkipDir
			}
			return nil
		}
		if !included(rel, cfg) || excluded(rel, cfg) {
			return nil
		}

		content, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("read %s: %w", filePath, err)
		}
		doc, ok := parseDocument(rel, content)
		if !ok {
			return nil
		}
		docs = append(docs, doc)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Path < docs[j].Path })
	if len(docs) == 0 {
		return nil, fmt.Errorf("no static documents found under %s; include=%v exclude=%v", root, cfg.Include, cfg.Exclude)
	}
	return docs, nil
}

func parseConfig(configJSON string) (Config, error) {
	configJSON = strings.TrimSpace(configJSON)
	cfg := Config{
		Include: []string{"**/*.md", "**/*.markdown", "**/*.html", "**/*.htm", "**/*.txt"},
		Exclude: []string{
			"**/.*/**",
			"**/node_modules/**",
			"**/vendor/**",
			"**/build/**",
			"**/dist/**",
			"**/out/**",
			"**/target/**",
			"**/coverage/**",
			"**/__pycache__/**",
		},
	}
	if configJSON == "" || configJSON == "{}" {
		return cfg, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(configJSON), &raw); err != nil {
		return Config{}, fmt.Errorf("parse static docs config_json: %w", err)
	}
	if include := stringList(raw["include"]); len(include) > 0 {
		cfg.Include = include
	}
	if exclude := stringList(raw["exclude"]); len(exclude) > 0 {
		cfg.Exclude = append(cfg.Exclude, exclude...)
	}
	return cfg, nil
}

func validateRoot(root string) error {
	root = strings.TrimSpace(root)
	if root == "" {
		return fmt.Errorf("static docs root is required")
	}
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("static docs root %q does not exist", root)
		}
		if os.IsPermission(err) {
			return fmt.Errorf("static docs root %q is not readable: %w", root, err)
		}
		return fmt.Errorf("stat static docs root %q: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("static docs root %q is not a directory", root)
	}
	return nil
}

func shouldSkipDir(name string, rel string, cfg Config) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch name {
	case "node_modules", "vendor", "build", "dist", "out", "target", "coverage", "__pycache__":
		return true
	}
	return excluded(strings.TrimSuffix(rel, "/")+"/", cfg)
}

func included(rel string, cfg Config) bool {
	for _, pattern := range cfg.Include {
		if matchPattern(pattern, rel) {
			return true
		}
	}
	return false
}

func excluded(rel string, cfg Config) bool {
	for _, pattern := range cfg.Exclude {
		if matchPattern(pattern, rel) {
			return true
		}
	}
	return false
}

func matchPattern(pattern string, rel string) bool {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	rel = filepath.ToSlash(strings.TrimPrefix(strings.TrimSpace(rel), "./"))
	if pattern == "" || rel == "" {
		return false
	}
	if strings.HasPrefix(pattern, "**/") && strings.HasSuffix(pattern, "/**") {
		middle := strings.TrimSuffix(strings.TrimPrefix(pattern, "**/"), "/**")
		if rel == middle || strings.HasPrefix(rel, middle+"/") || strings.Contains(rel, "/"+middle+"/") {
			return true
		}
	}
	if strings.HasPrefix(pattern, "**/") {
		suffix := strings.TrimPrefix(pattern, "**/")
		if matchPattern(suffix, rel) {
			return true
		}
	}
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
			return true
		}
	}
	if strings.Contains(pattern, "/**/") {
		parts := strings.SplitN(pattern, "/**/", 2)
		if strings.HasPrefix(rel, parts[0]+"/") && matchPattern(parts[1], strings.TrimPrefix(rel, parts[0]+"/")) {
			return true
		}
	}
	if ok, _ := path.Match(pattern, rel); ok {
		return true
	}
	if strings.Contains(pattern, "**") {
		parts := strings.Split(pattern, "**")
		if len(parts) == 2 {
			prefix := strings.Trim(parts[0], "/")
			suffix := strings.Trim(parts[1], "/")
			if prefix != "" && !strings.HasPrefix(rel, prefix+"/") && rel != prefix {
				return false
			}
			if suffix != "" {
				if ok, _ := path.Match(suffix, path.Base(rel)); ok {
					return true
				}
				return strings.HasSuffix(rel, suffix)
			}
			return true
		}
	}
	return false
}

func parseDocument(rel string, content []byte) (htmldocs.Document, bool) {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".html", ".htm":
		return htmldocs.Parse(rel, content), true
	case ".md", ".markdown":
		return markdownDocument(localdocs.Parse(rel, content)), true
	case ".txt":
		return textDocument(rel, content), true
	default:
		return htmldocs.Document{}, false
	}
}

func markdownDocument(doc localdocs.Document) htmldocs.Document {
	sections := make([]htmldocs.Section, 0, len(doc.Sections))
	for _, section := range doc.Sections {
		sections = append(sections, htmldocs.Section{
			HeadingPath: section.HeadingPath,
			Title:       section.Title,
			Content:     section.Content,
			Ordinal:     section.Ordinal,
			Hash:        section.Hash,
		})
	}
	return htmldocs.Document{
		Path:     doc.Path,
		Title:    doc.Title,
		Hash:     doc.Hash,
		Sections: sections,
	}
}

func textDocument(rel string, content []byte) htmldocs.Document {
	title := filepath.Base(rel)
	body := strings.TrimSpace(string(content))
	return htmldocs.Document{
		Path:  rel,
		Title: title,
		Hash:  hashBytes(content),
		Sections: []htmldocs.Section{
			{
				HeadingPath: []string{title},
				Title:       title,
				Content:     body,
				Ordinal:     0,
				Hash:        hashString(body),
			},
		},
	}
}

func stringList(value any) []string {
	switch typed := value.(type) {
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			if s, ok := item.(string); ok {
				for _, part := range strings.Split(s, ",") {
					if part = strings.TrimSpace(part); part != "" {
						values = append(values, part)
					}
				}
			}
		}
		return values
	case string:
		values := make([]string, 0)
		for _, part := range strings.Split(typed, ",") {
			if part = strings.TrimSpace(part); part != "" {
				values = append(values, part)
			}
		}
		return values
	default:
		return nil
	}
}

func hashString(value string) string {
	return hashBytes([]byte(value))
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
