package openapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Document struct {
	Path     string
	Title    string
	Hash     string
	Sections []Section
}

type Section struct {
	Title       string
	HeadingPath []string
	Content     string
	Ordinal     int
	Hash        string
}

func Load(path string) (Document, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return Document{}, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(filepath.ToSlash(path), content)
}

func Parse(name string, content []byte) (Document, error) {
	var root spec
	if err := unmarshalSpec(name, content, &root); err != nil {
		return Document{}, err
	}

	title := strings.TrimSpace(root.Info.Title)
	if title == "" {
		title = filepath.Base(name)
	}

	sections := make([]Section, 0)
	paths := make([]string, 0, len(root.Paths))
	for path := range root.Paths {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		methods := root.Paths[path]
		methodNames := make([]string, 0, len(methods))
		for method := range methods {
			if isHTTPMethod(method) {
				methodNames = append(methodNames, strings.ToUpper(method))
			}
		}
		sort.Strings(methodNames)

		for _, method := range methodNames {
			operation := methods[strings.ToLower(method)]
			sectionTitle := strings.TrimSpace(method + " " + path)
			content := operationContent(root, method, path, operation)
			sections = append(sections, Section{
				Title:       sectionTitle,
				HeadingPath: []string{title, "API", sectionTitle},
				Content:     content,
				Ordinal:     len(sections),
				Hash:        hashString(content),
			})
		}
	}

	return Document{
		Path:     name,
		Title:    title,
		Hash:     hashBytes(content),
		Sections: sections,
	}, nil
}

type spec struct {
	OpenAPI    string                          `json:"openapi" yaml:"openapi"`
	Swagger    string                          `json:"swagger" yaml:"swagger"`
	Info       info                            `json:"info" yaml:"info"`
	Paths      map[string]map[string]operation `json:"paths" yaml:"paths"`
	Components components                      `json:"components" yaml:"components"`
}

type info struct {
	Title       string `json:"title" yaml:"title"`
	Description string `json:"description" yaml:"description"`
	Version     string `json:"version" yaml:"version"`
}

type operation struct {
	OperationID string              `json:"operationId" yaml:"operationId"`
	Summary     string              `json:"summary" yaml:"summary"`
	Description string              `json:"description" yaml:"description"`
	Tags        []string            `json:"tags" yaml:"tags"`
	Parameters  []parameter         `json:"parameters" yaml:"parameters"`
	RequestBody requestBody         `json:"requestBody" yaml:"requestBody"`
	Responses   map[string]response `json:"responses" yaml:"responses"`
}

type components struct {
	Parameters    map[string]parameter   `json:"parameters" yaml:"parameters"`
	RequestBodies map[string]requestBody `json:"requestBodies" yaml:"requestBodies"`
	Responses     map[string]response    `json:"responses" yaml:"responses"`
	Schemas       map[string]schema      `json:"schemas" yaml:"schemas"`
}

type parameter struct {
	Ref         string `json:"$ref" yaml:"$ref"`
	Name        string `json:"name" yaml:"name"`
	In          string `json:"in" yaml:"in"`
	Description string `json:"description" yaml:"description"`
	Required    bool   `json:"required" yaml:"required"`
	Schema      schema `json:"schema" yaml:"schema"`
}

type requestBody struct {
	Ref         string               `json:"$ref" yaml:"$ref"`
	Description string               `json:"description" yaml:"description"`
	Required    bool                 `json:"required" yaml:"required"`
	Content     map[string]mediaType `json:"content" yaml:"content"`
}

type response struct {
	Ref         string               `json:"$ref" yaml:"$ref"`
	Description string               `json:"description" yaml:"description"`
	Content     map[string]mediaType `json:"content" yaml:"content"`
}

type mediaType struct {
	Schema schema `json:"schema" yaml:"schema"`
}

type schema struct {
	Ref         string            `json:"$ref" yaml:"$ref"`
	Type        string            `json:"type" yaml:"type"`
	Format      string            `json:"format" yaml:"format"`
	Description string            `json:"description" yaml:"description"`
	Items       *schema           `json:"items" yaml:"items"`
	Properties  map[string]schema `json:"properties" yaml:"properties"`
}

func unmarshalSpec(name string, content []byte, root *spec) error {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == ".yaml" || ext == ".yml" {
		if err := yaml.Unmarshal(content, root); err != nil {
			return fmt.Errorf("parse OpenAPI YAML: %w", err)
		}
		return nil
	}
	if err := json.Unmarshal(content, root); err != nil {
		return fmt.Errorf("parse OpenAPI JSON: %w", err)
	}
	return nil
}

func operationContent(root spec, method, path string, operation operation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", strings.ToUpper(method), path)
	if operation.OperationID != "" {
		fmt.Fprintf(&b, "operationId: %s\n", operation.OperationID)
	}
	if len(operation.Tags) > 0 {
		fmt.Fprintf(&b, "tags: %s\n", strings.Join(operation.Tags, ", "))
	}
	if operation.Summary != "" {
		fmt.Fprintf(&b, "summary: %s\n", strings.TrimSpace(operation.Summary))
	}
	if operation.Description != "" {
		fmt.Fprintf(&b, "description: %s\n", strings.TrimSpace(operation.Description))
	}
	writeParameters(&b, root, operation.Parameters)
	writeRequestBody(&b, root, operation.RequestBody)
	writeResponses(&b, root, operation.Responses)
	return b.String()
}

func writeParameters(b *strings.Builder, root spec, parameters []parameter) {
	if len(parameters) == 0 {
		return
	}
	fmt.Fprintln(b, "parameters:")
	for _, param := range parameters {
		param = resolveParameter(root, param)
		name := strings.TrimSpace(param.Name)
		if name == "" {
			name = localRefName(param.Ref)
		}
		where := strings.TrimSpace(param.In)
		if where == "" {
			where = "unknown"
		}
		required := ""
		if param.Required {
			required = " required"
		}
		fmt.Fprintf(b, "- %s in %s%s", name, where, required)
		if label := schemaLabel(root, param.Schema); label != "" {
			fmt.Fprintf(b, " schema: %s", label)
		}
		if param.Description != "" {
			fmt.Fprintf(b, " description: %s", strings.TrimSpace(param.Description))
		}
		fmt.Fprintln(b)
	}
}

func writeRequestBody(b *strings.Builder, root spec, body requestBody) {
	body = resolveRequestBody(root, body)
	if body.Description == "" && len(body.Content) == 0 {
		return
	}
	required := ""
	if body.Required {
		required = " required"
	}
	fmt.Fprintf(b, "requestBody:%s", required)
	if body.Description != "" {
		fmt.Fprintf(b, " %s", strings.TrimSpace(body.Description))
	}
	fmt.Fprintln(b)
	writeContentSchemas(b, root, body.Content)
}

func writeResponses(b *strings.Builder, root spec, responses map[string]response) {
	if len(responses) == 0 {
		return
	}
	codes := make([]string, 0, len(responses))
	for code := range responses {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	fmt.Fprintln(b, "responses:")
	for _, code := range codes {
		resp := resolveResponse(root, responses[code])
		fmt.Fprintf(b, "- %s", code)
		if resp.Description != "" {
			fmt.Fprintf(b, " %s", strings.TrimSpace(resp.Description))
		}
		fmt.Fprintln(b)
		writeContentSchemas(b, root, resp.Content)
	}
}

func writeContentSchemas(b *strings.Builder, root spec, content map[string]mediaType) {
	if len(content) == 0 {
		return
	}
	mediaTypes := make([]string, 0, len(content))
	for media := range content {
		mediaTypes = append(mediaTypes, media)
	}
	sort.Strings(mediaTypes)
	for _, media := range mediaTypes {
		if label := schemaLabel(root, content[media].Schema); label != "" {
			fmt.Fprintf(b, "  content %s schema: %s\n", media, label)
		}
	}
}

func resolveParameter(root spec, param parameter) parameter {
	if param.Ref == "" {
		return param
	}
	if name, ok := localComponentName(param.Ref, "#/components/parameters/"); ok {
		resolved := root.Components.Parameters[name]
		resolved.Ref = param.Ref
		return resolved
	}
	return param
}

func resolveRequestBody(root spec, body requestBody) requestBody {
	if body.Ref == "" {
		return body
	}
	if name, ok := localComponentName(body.Ref, "#/components/requestBodies/"); ok {
		resolved := root.Components.RequestBodies[name]
		resolved.Ref = body.Ref
		return resolved
	}
	return body
}

func resolveResponse(root spec, resp response) response {
	if resp.Ref == "" {
		return resp
	}
	if name, ok := localComponentName(resp.Ref, "#/components/responses/"); ok {
		resolved := root.Components.Responses[name]
		resolved.Ref = resp.Ref
		return resolved
	}
	return resp
}

func schemaLabel(root spec, value schema) string {
	if value.Ref != "" {
		if name, ok := localComponentName(value.Ref, "#/components/schemas/"); ok {
			resolved := root.Components.Schemas[name]
			label := name
			if resolved.Type != "" {
				label += " " + resolved.Type
			}
			if resolved.Description != "" {
				label += " " + strings.TrimSpace(resolved.Description)
			}
			return strings.TrimSpace(label)
		}
		return localRefName(value.Ref)
	}
	parts := make([]string, 0, 3)
	if value.Type != "" {
		parts = append(parts, value.Type)
	}
	if value.Format != "" {
		parts = append(parts, value.Format)
	}
	if value.Description != "" {
		parts = append(parts, strings.TrimSpace(value.Description))
	}
	if value.Items != nil {
		if item := schemaLabel(root, *value.Items); item != "" {
			parts = append(parts, "items "+item)
		}
	}
	return strings.Join(parts, " ")
}

func localComponentName(ref string, prefix string) (string, bool) {
	if !strings.HasPrefix(ref, prefix) {
		return "", false
	}
	name := strings.TrimSpace(strings.TrimPrefix(ref, prefix))
	return name, name != ""
}

func localRefName(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

func isHTTPMethod(method string) bool {
	switch strings.ToLower(method) {
	case "get", "post", "put", "patch", "delete", "options", "head", "trace":
		return true
	default:
		return false
	}
}

func hashString(value string) string {
	return hashBytes([]byte(value))
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
