package extract

import (
	"strings"
	"unicode"
)

func CanonicalMethod(method string) string {
	method = strings.ToUpper(strings.TrimSpace(method))
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return method
	default:
		return ""
	}
}

func CanonicalPath(path string) string {
	path = trimSurroundingPunctuation(path)
	if path == "" || path == "/" || !strings.HasPrefix(path, "/") {
		return ""
	}
	return collapsePlaceholderWhitespace(path)
}

func CanonicalOperation(operation string) string {
	return strings.TrimSpace(strings.Trim(operation, "\"'`,;.!?()[]{}<>"))
}

func scanPathCandidates(text string) []pathCandidate {
	runes := []rune(text)
	byteOffsets := make([]int, 0, len(runes)+1)
	for idx := range text {
		byteOffsets = append(byteOffsets, idx)
	}
	byteOffsets = append(byteOffsets, len(text))

	paths := make([]pathCandidate, 0)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '/' || i+1 >= len(runes) || runes[i+1] == '/' || isPathDelimiter(runes[i+1], 0) {
			continue
		}
		startRune := i
		braceDepth := 0
		i++
		for i < len(runes) {
			r := runes[i]
			if r == '{' {
				braceDepth++
				i++
				continue
			}
			if r == '}' && braceDepth > 0 {
				braceDepth--
				i++
				continue
			}
			if isPathDelimiter(r, braceDepth) {
				break
			}
			i++
		}
		raw := text[byteOffsets[startRune]:byteOffsets[i]]
		canonical := CanonicalPath(raw)
		if canonical != "" {
			paths = append(paths, pathCandidate{
				Raw:       trimSurroundingPunctuation(raw),
				Canonical: canonical,
				Start:     byteOffsets[startRune],
				End:       byteOffsets[i],
			})
		}
	}
	return paths
}

func isPathDelimiter(r rune, braceDepth int) bool {
	if braceDepth > 0 {
		return strings.ContainsRune("\"'`|\\<>", r)
	}
	return unicode.IsSpace(r) || strings.ContainsRune("\"'`,;|\\<>", r)
}

func trimSurroundingPunctuation(value string) string {
	value = strings.TrimSpace(value)
	return strings.Trim(value, "\"'`,;.!?()[]<>")
}

func collapsePlaceholderWhitespace(path string) string {
	var out strings.Builder
	inPlaceholder := false
	pendingSpace := false
	for _, r := range path {
		switch {
		case r == '{':
			inPlaceholder = true
			pendingSpace = false
			out.WriteRune(r)
		case r == '}':
			inPlaceholder = false
			pendingSpace = false
			out.WriteRune(r)
		case inPlaceholder && unicode.IsSpace(r):
			pendingSpace = true
		default:
			if pendingSpace && out.Len() > 0 {
				// Whitespace inside placeholders is intentionally dropped:
				// "{ object.id }" and "{object.id}" refer to the same route token.
				pendingSpace = false
			}
			out.WriteRune(r)
		}
	}
	return out.String()
}
