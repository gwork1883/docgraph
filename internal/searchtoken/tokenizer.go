package searchtoken

import (
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/go-ego/gse"
)

type Term struct {
	Text   string
	Source string
}

type Tokenizer interface {
	QueryTerms(text string) []Term
	IndexTerms(text string) []string
	SymbolTerms(text string) []string
}

type DictionaryEntry struct {
	Text string
	Freq float64
	Pos  string
}

type Options struct {
	DictionaryFiles   []string
	DictionaryEntries []DictionaryEntry
}

type GSETokenizer struct {
	seg gse.Segmenter
}

var identifierPattern = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_.-]{2,}`)

var (
	defaultOnce      sync.Once
	defaultTokenizer Tokenizer
)

func Default() Tokenizer {
	defaultOnce.Do(func() {
		tokenizer, err := NewGSETokenizer(Options{})
		if err != nil {
			defaultTokenizer = ruleTokenizer{}
			return
		}
		defaultTokenizer = tokenizer
	})
	return defaultTokenizer
}

func NewGSETokenizer(opts Options) (*GSETokenizer, error) {
	tokenizer := &GSETokenizer{}
	tokenizer.seg.AlphaNum = true
	if len(opts.DictionaryFiles) > 0 {
		if err := tokenizer.seg.LoadDict(opts.DictionaryFiles...); err != nil {
			return nil, err
		}
	} else if err := tokenizer.seg.LoadDict(); err != nil {
		return nil, err
	}
	for _, entry := range opts.DictionaryEntries {
		text := strings.TrimSpace(entry.Text)
		if text == "" {
			continue
		}
		freq := entry.Freq
		if freq <= 0 {
			freq = 100
		}
		if entry.Pos != "" {
			if err := tokenizer.seg.AddToken(text, freq, entry.Pos); err != nil {
				return nil, err
			}
		} else if err := tokenizer.seg.AddToken(text, freq); err != nil {
			return nil, err
		}
	}
	return tokenizer, nil
}

func (t *GSETokenizer) QueryTerms(text string) []Term {
	terms := make([]Term, 0)
	add := func(value string, source string) {
		value = normalizeTerm(value)
		if value == "" {
			return
		}
		terms = append(terms, Term{Text: value, Source: source})
	}
	add(text, "raw")
	for _, token := range t.seg.CutSearch(text, true) {
		add(token, "gse")
	}
	for _, token := range t.SymbolTerms(text) {
		add(token, "symbol")
	}
	return uniqueTerms(terms)
}

func (t *GSETokenizer) IndexTerms(text string) []string {
	terms := make([]string, 0)
	terms = append(terms, text)
	for _, token := range t.seg.CutSearch(text, true) {
		terms = append(terms, normalizeTerm(token))
	}
	terms = append(terms, t.SymbolTerms(text)...)
	return uniqueStrings(nonEmptyStrings(terms))
}

func (t *GSETokenizer) SymbolTerms(text string) []string {
	return symbolTerms(text)
}

type ruleTokenizer struct{}

func (ruleTokenizer) QueryTerms(text string) []Term {
	terms := make([]Term, 0)
	add := func(value string, source string) {
		value = normalizeTerm(value)
		if value != "" {
			terms = append(terms, Term{Text: value, Source: source})
		}
	}
	add(text, "raw")
	for _, token := range asciiTokens(text) {
		add(token, "rule")
	}
	for _, token := range symbolTerms(text) {
		add(token, "symbol")
	}
	return uniqueTerms(terms)
}

func (ruleTokenizer) IndexTerms(text string) []string {
	terms := []string{text}
	terms = append(terms, asciiTokens(text)...)
	terms = append(terms, symbolTerms(text)...)
	return uniqueStrings(nonEmptyStrings(terms))
}

func (ruleTokenizer) SymbolTerms(text string) []string {
	return symbolTerms(text)
}

func symbolTerms(text string) []string {
	raw := identifierPattern.FindAllString(text, -1)
	terms := make([]string, 0, len(raw)*3)
	for _, token := range raw {
		token = strings.Trim(token, `"'.,;:!?()[]{}<>`)
		if token == "" || isLanguageKeyword(strings.ToLower(token)) {
			continue
		}
		terms = append(terms, token, strings.ToLower(token))
		for _, part := range splitIdentifier(token) {
			if part != "" && !isLanguageKeyword(strings.ToLower(part)) {
				terms = append(terms, part, strings.ToLower(part))
			}
		}
	}
	return uniqueStrings(nonEmptyStrings(terms))
}

func asciiTokens(text string) []string {
	terms := make([]string, 0)
	var token strings.Builder
	flush := func() {
		if token.Len() == 0 {
			return
		}
		value := strings.Trim(strings.ToLower(token.String()), `"'.,;:!?()[]{}<>`)
		token.Reset()
		if len(value) >= 2 {
			terms = append(terms, value)
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("/_.-", r) {
			token.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return terms
}

func splitIdentifier(value string) []string {
	value = strings.ReplaceAll(value, "-", "_")
	value = strings.ReplaceAll(value, ".", "_")
	parts := make([]string, 0)
	for _, chunk := range strings.FieldsFunc(value, func(r rune) bool { return r == '_' || r == '/' }) {
		var current []rune
		flush := func() {
			if len(current) > 0 {
				parts = append(parts, string(current))
				current = nil
			}
		}
		runes := []rune(chunk)
		for i, r := range runes {
			if i > 0 && unicode.IsUpper(r) {
				prev := runes[i-1]
				nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
				if unicode.IsLower(prev) || unicode.IsDigit(prev) || nextLower {
					flush()
				}
			}
			current = append(current, r)
		}
		flush()
	}
	return parts
}

func normalizeTerm(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, `"'.,;:!?()[]{}<>`)
	if value == "" {
		return ""
	}
	return value
}

func uniqueTerms(terms []Term) []Term {
	seen := map[string]bool{}
	result := make([]Term, 0, len(terms))
	for _, term := range terms {
		key := strings.ToLower(term.Text) + "\x00" + term.Source
		if term.Text == "" || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, term)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if len([]rune(result[i].Text)) == len([]rune(result[j].Text)) {
			return result[i].Text < result[j].Text
		}
		return len([]rune(result[i].Text)) > len([]rune(result[j].Text))
	})
	return result
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = normalizeTerm(value)
		key := strings.ToLower(value)
		if value == "" || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, value)
	}
	return result
}

func nonEmptyStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func isLanguageKeyword(value string) bool {
	switch value {
	case "and", "as", "break", "case", "catch", "class", "const", "continue", "def", "default", "defer", "delete", "do", "else", "enum", "export", "extends", "false", "final", "finally", "for", "func", "function", "go", "if", "import", "in", "interface", "let", "map", "new", "nil", "null", "package", "private", "protected", "public", "return", "select", "static", "struct", "switch", "this", "throw", "true", "try", "type", "var", "void", "while", "with":
		return true
	default:
		return false
	}
}
