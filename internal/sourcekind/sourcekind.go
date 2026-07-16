package sourcekind

import (
	"fmt"
	"strings"
)

var supported = map[string]struct{}{
	"confluence": {},
	"git":        {},
	"html":       {},
	"local":      {},
	"openapi":    {},
	"sftp":       {},
	"static":     {},
	"webdocs":    {},
	"xmind":      {},
}

func Supported(kind string) bool {
	_, ok := supported[strings.TrimSpace(kind)]
	return ok
}

func Validate(kind string) error {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return fmt.Errorf("source kind is required")
	}
	if !Supported(kind) {
		return fmt.Errorf("unsupported source kind %q", kind)
	}
	return nil
}
