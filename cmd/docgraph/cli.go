package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/docgraph/docgraph/internal/config"
	"github.com/docgraph/docgraph/internal/ids"
	"github.com/docgraph/docgraph/internal/query"
	"github.com/docgraph/docgraph/internal/storage"
	syncsvc "github.com/docgraph/docgraph/internal/sync"
)

func runSource(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("source command requires add, list, update, delete, sync, or jobs")
	}
	switch args[0] {
	case "add":
		return runSourceAdd(args[1:])
	case "list":
		return runSourceList(args[1:])
	case "update":
		return runSourceUpdate(args[1:])
	case "delete":
		return runSourceDelete(args[1:])
	case "sync":
		return runSourceSync(args[1:])
	case "jobs":
		return runSourceJobs(args[1:])
	default:
		return fmt.Errorf("unknown source command %q", args[0])
	}
}

func runSourceAdd(args []string) error {
	fs := flag.NewFlagSet("source add", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	dataDir := fs.String("data", "", "data directory")
	name := fs.String("name", "", "source name")
	kind := fs.String("kind", "local", "source kind")
	dsn := fs.String("dsn", "", "source DSN or local path")
	product := fs.String("product", "", "product hint")
	module := fs.String("module", "", "module hint")
	branch := fs.String("branch", "", "git branch")
	sourcePath := fs.String("path", "", "git source path")
	cache := fs.String("cache", "", "explicit clone directory for remote Git sources")
	include := fs.String("include", "", "include glob list for static docs")
	exclude := fs.String("exclude", "", "exclude glob list for static docs")
	urlPrefix := fs.String("url-prefix", "", "public URL prefix for static docs")
	identityFile := fs.String("identity-file", "", "SFTP identity file")
	password := fs.String("password", "", "SFTP password")
	passphrase := fs.String("passphrase", "", "SFTP identity file passphrase")
	knownHosts := fs.String("known-hosts", "", "SFTP known_hosts file")
	strictHostKey := fs.Bool("strict-host-key", false, "require SFTP host key verification")
	baseURL := fs.String("base-url", "", "Confluence base URL")
	pageID := fs.String("page-id", "", "Confluence page id")
	spaceKey := fs.String("space-key", "", "Confluence space key")
	token := fs.String("token", "", "Confluence bearer token")
	username := fs.String("username", "", "Confluence username")
	apiToken := fs.String("api-token", "", "Confluence API token")
	includeChildren := fs.Bool("include-children", false, "include Confluence child pages")
	maxPages := fs.String("max-pages", "", "maximum pages for webdocs crawl")
	maxDepth := fs.String("max-depth", "", "maximum crawl depth for webdocs crawl")
	bearerToken := fs.String("bearer-token", "", "webdocs bearer token")
	cookie := fs.String("cookie", "", "webdocs cookie header")
	headersJSON := fs.String("headers-json", "", "webdocs custom headers JSON")
	isSPA := fs.Bool("is-spa", false, "mark webdocs source as single-page app requiring browser rendering")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return fmt.Errorf("--name is required")
	}
	if *dsn == "" {
		return fmt.Errorf("--dsn is required")
	}

	store, closeStore, err := openStoreFromFlags(*cfgPath, *dataDir)
	if err != nil {
		return err
	}
	defer closeStore()

	source, err := store.CreateSource(context.Background(), storage.Source{
		ID:   ids.Random("src", 12),
		Kind: strings.TrimSpace(*kind),
		Name: strings.TrimSpace(*name),
		DSN:  strings.TrimSpace(*dsn),
		ConfigJSON: sourceConfigJSON(map[string]string{
			"branch":        *branch,
			"path":          *sourcePath,
			"cache":         *cache,
			"include":       *include,
			"exclude":       *exclude,
			"url_prefix":    *urlPrefix,
			"identity_file": *identityFile,
			"password":      *password,
			"passphrase":    *passphrase,
			"known_hosts":   *knownHosts,
			"base_url":      *baseURL,
			"page_id":       *pageID,
			"space_key":     *spaceKey,
			"token":         *token,
			"username":      *username,
			"api_token":     *apiToken,
			"max_pages":     *maxPages,
			"max_depth":     *maxDepth,
			"bearer_token":  *bearerToken,
			"cookie":        *cookie,
			"headers_json":  *headersJSON,
		}, map[string]bool{
			"include_children": *includeChildren,
			"strict_host_key":  *strictHostKey,
			"is_spa":           *isSPA,
		}),
		ProductHint: strings.TrimSpace(*product),
		ModuleHint:  strings.TrimSpace(*module),
	})
	if err != nil {
		return err
	}
	return printJSON(source)
}

func runSourceList(args []string) error {
	fs := flag.NewFlagSet("source list", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	dataDir := fs.String("data", "", "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	store, closeStore, err := openStoreFromFlags(*cfgPath, *dataDir)
	if err != nil {
		return err
	}
	defer closeStore()

	sources, err := store.ListSources(context.Background())
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"sources": sources})
}

func runSourceUpdate(args []string) error {
	fs := flag.NewFlagSet("source update", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	dataDir := fs.String("data", "", "data directory")
	id := fs.String("id", "", "source id")
	name := fs.String("name", "", "source name")
	kind := fs.String("kind", "", "source kind")
	dsn := fs.String("dsn", "", "source DSN or local path")
	product := fs.String("product", "", "product hint")
	module := fs.String("module", "", "module hint")
	branch := fs.String("branch", "", "git branch")
	sourcePath := fs.String("path", "", "git source path")
	cache := fs.String("cache", "", "explicit clone directory for remote Git sources")
	include := fs.String("include", "", "include glob list for static docs")
	exclude := fs.String("exclude", "", "exclude glob list for static docs")
	urlPrefix := fs.String("url-prefix", "", "public URL prefix for static docs")
	identityFile := fs.String("identity-file", "", "SFTP identity file")
	password := fs.String("password", "", "SFTP password")
	passphrase := fs.String("passphrase", "", "SFTP identity file passphrase")
	knownHosts := fs.String("known-hosts", "", "SFTP known_hosts file")
	strictHostKey := fs.Bool("strict-host-key", false, "require SFTP host key verification")
	baseURL := fs.String("base-url", "", "Confluence base URL")
	pageID := fs.String("page-id", "", "Confluence page id")
	spaceKey := fs.String("space-key", "", "Confluence space key")
	token := fs.String("token", "", "Confluence bearer token")
	username := fs.String("username", "", "Confluence username")
	apiToken := fs.String("api-token", "", "Confluence API token")
	includeChildren := fs.Bool("include-children", false, "include Confluence child pages")
	maxPages := fs.String("max-pages", "", "maximum pages for webdocs crawl")
	maxDepth := fs.String("max-depth", "", "maximum crawl depth for webdocs crawl")
	bearerToken := fs.String("bearer-token", "", "webdocs bearer token")
	cookie := fs.String("cookie", "", "webdocs cookie header")
	headersJSON := fs.String("headers-json", "", "webdocs custom headers JSON")
	isSPA := fs.Bool("is-spa", false, "mark webdocs source as single-page app requiring browser rendering")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sourceID := strings.TrimSpace(*id)
	if sourceID == "" {
		return fmt.Errorf("--id is required")
	}

	store, closeStore, err := openStoreFromFlags(*cfgPath, *dataDir)
	if err != nil {
		return err
	}
	defer closeStore()

	source, err := store.GetSource(context.Background(), sourceID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*kind) != "" {
		source.Kind = strings.TrimSpace(*kind)
	}
	if strings.TrimSpace(*name) != "" {
		source.Name = strings.TrimSpace(*name)
	}
	if strings.TrimSpace(*dsn) != "" {
		source.DSN = strings.TrimSpace(*dsn)
	}
	if strings.TrimSpace(*product) != "" {
		source.ProductHint = strings.TrimSpace(*product)
	}
	if strings.TrimSpace(*module) != "" {
		source.ModuleHint = strings.TrimSpace(*module)
	}
	if strings.TrimSpace(*branch) != "" || strings.TrimSpace(*sourcePath) != "" || strings.TrimSpace(*cache) != "" || strings.TrimSpace(*include) != "" || strings.TrimSpace(*exclude) != "" || strings.TrimSpace(*urlPrefix) != "" || strings.TrimSpace(*identityFile) != "" || strings.TrimSpace(*password) != "" || strings.TrimSpace(*passphrase) != "" || strings.TrimSpace(*knownHosts) != "" || *strictHostKey {
		source.ConfigJSON = mergeSourceConfigJSON(source.ConfigJSON, map[string]string{
			"branch":        strings.TrimSpace(*branch),
			"path":          strings.TrimSpace(*sourcePath),
			"cache":         strings.TrimSpace(*cache),
			"include":       strings.TrimSpace(*include),
			"exclude":       strings.TrimSpace(*exclude),
			"url_prefix":    strings.TrimSpace(*urlPrefix),
			"identity_file": strings.TrimSpace(*identityFile),
			"password":      strings.TrimSpace(*password),
			"passphrase":    strings.TrimSpace(*passphrase),
			"known_hosts":   strings.TrimSpace(*knownHosts),
		}, map[string]bool{"strict_host_key": *strictHostKey})
	}
	if strings.TrimSpace(*baseURL) != "" || strings.TrimSpace(*pageID) != "" || strings.TrimSpace(*spaceKey) != "" || strings.TrimSpace(*token) != "" || strings.TrimSpace(*username) != "" || strings.TrimSpace(*apiToken) != "" || strings.TrimSpace(*maxPages) != "" || strings.TrimSpace(*maxDepth) != "" || strings.TrimSpace(*bearerToken) != "" || strings.TrimSpace(*cookie) != "" || strings.TrimSpace(*headersJSON) != "" || *includeChildren {
		source.ConfigJSON = mergeSourceConfigJSON(source.ConfigJSON, map[string]string{
			"base_url":     strings.TrimSpace(*baseURL),
			"page_id":      strings.TrimSpace(*pageID),
			"space_key":    strings.TrimSpace(*spaceKey),
			"token":        strings.TrimSpace(*token),
			"username":     strings.TrimSpace(*username),
			"api_token":    strings.TrimSpace(*apiToken),
			"max_pages":    strings.TrimSpace(*maxPages),
			"max_depth":    strings.TrimSpace(*maxDepth),
			"bearer_token": strings.TrimSpace(*bearerToken),
			"cookie":       strings.TrimSpace(*cookie),
			"headers_json": strings.TrimSpace(*headersJSON),
		}, map[string]bool{"include_children": *includeChildren, "is_spa": *isSPA})
	}

	updated, err := store.UpdateSource(context.Background(), source)
	if err != nil {
		return err
	}
	return printJSON(updated)
}

func runSourceDelete(args []string) error {
	fs := flag.NewFlagSet("source delete", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	dataDir := fs.String("data", "", "data directory")
	id := fs.String("id", "", "source id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sourceID := strings.TrimSpace(*id)
	if sourceID == "" {
		return fmt.Errorf("--id is required")
	}

	store, closeStore, err := openStoreFromFlags(*cfgPath, *dataDir)
	if err != nil {
		return err
	}
	defer closeStore()

	if err := store.DeleteSource(context.Background(), sourceID); err != nil {
		return err
	}
	return printJSON(map[string]string{"deleted": sourceID})
}

func runSourceSync(args []string) error {
	fs := flag.NewFlagSet("source sync", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	dataDir := fs.String("data", "", "data directory")
	id := fs.String("id", "", "source id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("--id is required")
	}

	store, closeStore, err := openStoreFromFlags(*cfgPath, *dataDir)
	if err != nil {
		return err
	}
	defer closeStore()

	result, err := syncsvc.NewService(store).SyncSource(context.Background(), *id)
	if err != nil {
		return err
	}
	return printJSON(result)
}

func runSourceJobs(args []string) error {
	fs := flag.NewFlagSet("source jobs", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	dataDir := fs.String("data", "", "data directory")
	id := fs.String("id", "", "source id")
	limit := fs.Int("limit", 20, "job history limit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sourceID := strings.TrimSpace(*id)
	if sourceID == "" {
		return fmt.Errorf("--id is required")
	}

	store, closeStore, err := openStoreFromFlags(*cfgPath, *dataDir)
	if err != nil {
		return err
	}
	defer closeStore()

	jobs, err := store.ListSyncJobs(context.Background(), sourceID, *limit)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"jobs": jobs})
}

func sourceConfigJSON(stringsMap map[string]string, boolsMap map[string]bool) string {
	return mergeSourceConfigJSON("{}", stringsMap, boolsMap)
}

func mergeSourceConfigJSON(existing string, stringsMap map[string]string, boolsMap map[string]bool) string {
	value := map[string]any{}
	if strings.TrimSpace(existing) != "" && strings.TrimSpace(existing) != "{}" {
		_ = json.Unmarshal([]byte(existing), &value)
	}
	for key, raw := range stringsMap {
		if strings.TrimSpace(raw) != "" {
			value[key] = strings.TrimSpace(raw)
		}
	}
	for key, raw := range boolsMap {
		value[key] = raw
	}
	if len(value) == 0 {
		return "{}"
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func runSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	dataDir := fs.String("data", "", "data directory")
	limit := fs.Int("limit", 10, "result limit")
	if err := fs.Parse(normalizeInterspersedFlags(args, map[string]bool{
		"config": true,
		"data":   true,
		"limit":  true,
	})); err != nil {
		return err
	}
	queryText := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if queryText == "" {
		return fmt.Errorf("search query is required")
	}

	store, closeStore, err := openStoreFromFlags(*cfgPath, *dataDir)
	if err != nil {
		return err
	}
	defer closeStore()

	hits, err := query.NewService(store).Search(context.Background(), queryText, *limit)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"hits": hits})
}

func runContext(args []string) error {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	dataDir := fs.String("data", "", "data directory")
	maxSections := fs.Int("max-sections", 8, "maximum sections")
	maxChars := fs.Int("max-chars", 12000, "maximum content characters")
	if err := fs.Parse(normalizeInterspersedFlags(args, map[string]bool{
		"config":       true,
		"data":         true,
		"max-sections": true,
		"max-chars":    true,
	})); err != nil {
		return err
	}
	task := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if task == "" {
		return fmt.Errorf("context task is required")
	}

	store, closeStore, err := openStoreFromFlags(*cfgPath, *dataDir)
	if err != nil {
		return err
	}
	defer closeStore()

	pack, err := query.NewService(store).Context(context.Background(), query.ContextRequest{
		Task:        task,
		MaxSections: *maxSections,
		MaxChars:    *maxChars,
	})
	if err != nil {
		return err
	}
	return printJSON(pack)
}

func runNode(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("node command requires get or related")
	}
	switch args[0] {
	case "get":
		return runNodeGet(args[1:])
	case "related":
		return runNodeRelated(args[1:])
	default:
		return fmt.Errorf("unknown node command %q", args[0])
	}
}

func runNodeGet(args []string) error {
	fs := flag.NewFlagSet("node get", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	dataDir := fs.String("data", "", "data directory")
	id := fs.String("id", "", "node id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*id) == "" {
		return fmt.Errorf("--id is required")
	}

	store, closeStore, err := openStoreFromFlags(*cfgPath, *dataDir)
	if err != nil {
		return err
	}
	defer closeStore()

	node, err := store.GetNode(context.Background(), strings.TrimSpace(*id))
	if err != nil {
		return err
	}
	return printJSON(node)
}

func runNodeRelated(args []string) error {
	fs := flag.NewFlagSet("node related", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	dataDir := fs.String("data", "", "data directory")
	id := fs.String("id", "", "node id")
	direction := fs.String("direction", "", "edge direction: both, out, or in")
	kind := fs.String("kind", "", "edge kind filter")
	limit := fs.Int("limit", 0, "related node limit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*id) == "" {
		return fmt.Errorf("--id is required")
	}

	store, closeStore, err := openStoreFromFlags(*cfgPath, *dataDir)
	if err != nil {
		return err
	}
	defer closeStore()

	related, err := store.RelatedNodes(context.Background(), strings.TrimSpace(*id), storage.RelatedOptions{
		Direction: strings.TrimSpace(*direction),
		Kind:      strings.TrimSpace(*kind),
		Limit:     *limit,
	})
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"related": related})
}

func runImpact(args []string) error {
	fs := flag.NewFlagSet("impact", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	dataDir := fs.String("data", "", "data directory")
	id := fs.String("id", "", "node id")
	direction := fs.String("direction", "out", "edge direction: out, in, or both")
	kind := fs.String("kind", "", "edge kind filter")
	maxDepth := fs.Int("max-depth", 2, "maximum traversal depth")
	limit := fs.Int("limit", 50, "maximum paths")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*id) == "" {
		return fmt.Errorf("--id is required")
	}

	store, closeStore, err := openStoreFromFlags(*cfgPath, *dataDir)
	if err != nil {
		return err
	}
	defer closeStore()

	result, err := store.Impact(context.Background(), strings.TrimSpace(*id), storage.ImpactOptions{
		Direction: strings.TrimSpace(*direction),
		Kind:      strings.TrimSpace(*kind),
		MaxDepth:  *maxDepth,
		Limit:     *limit,
	})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func runFeedback(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("feedback command requires add or list")
	}
	switch args[0] {
	case "add":
		return runFeedbackAdd(args[1:])
	case "list":
		return runFeedbackList(args[1:])
	default:
		return fmt.Errorf("unknown feedback command %q", args[0])
	}
}

func runFeedbackAdd(args []string) error {
	fs := flag.NewFlagSet("feedback add", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	dataDir := fs.String("data", "", "data directory")
	targetKind := fs.String("target-kind", "", "target kind: document, edge, or node")
	targetID := fs.String("target-id", "", "target id")
	feedbackKind := fs.String("kind", "", "feedback kind")
	actor := fs.String("actor", "", "actor")
	payload := fs.String("payload", "{}", "JSON payload")
	if err := fs.Parse(args); err != nil {
		return err
	}

	store, closeStore, err := openStoreFromFlags(*cfgPath, *dataDir)
	if err != nil {
		return err
	}
	defer closeStore()

	event, err := store.CreateFeedbackEvent(context.Background(), storage.FeedbackEventInput{
		TargetKind:   strings.TrimSpace(*targetKind),
		TargetID:     strings.TrimSpace(*targetID),
		FeedbackKind: strings.TrimSpace(*feedbackKind),
		PayloadJSON:  strings.TrimSpace(*payload),
		Actor:        strings.TrimSpace(*actor),
	})
	if err != nil {
		return err
	}
	return printJSON(event)
}

func runFeedbackList(args []string) error {
	fs := flag.NewFlagSet("feedback list", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	dataDir := fs.String("data", "", "data directory")
	targetKind := fs.String("target-kind", "", "target kind filter")
	targetID := fs.String("target-id", "", "target id filter")
	feedbackKind := fs.String("kind", "", "feedback kind filter")
	limit := fs.Int("limit", 20, "feedback event limit")
	if err := fs.Parse(args); err != nil {
		return err
	}

	store, closeStore, err := openStoreFromFlags(*cfgPath, *dataDir)
	if err != nil {
		return err
	}
	defer closeStore()

	events, err := store.ListFeedbackEvents(context.Background(), storage.FeedbackListOptions{
		TargetKind:   strings.TrimSpace(*targetKind),
		TargetID:     strings.TrimSpace(*targetID),
		FeedbackKind: strings.TrimSpace(*feedbackKind),
		Limit:        *limit,
	})
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"feedback": events})
}

func openStoreFromFlags(cfgPath, dataDir string) (storage.Store, func(), error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	if dataDir != "" {
		cfg.Server.DataDir = dataDir
		cfg.Storage.DSN = "sqlite://" + filepath.ToSlash(filepath.Join(dataDir, "docgraph.db"))
	}

	store, err := storage.Open(context.Background(), cfg.Storage.DSN)
	if err != nil {
		return nil, nil, err
	}
	if err := store.Migrate(context.Background()); err != nil {
		_ = store.Close()
		return nil, nil, err
	}
	return store, func() {
		if err := store.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "close store: %v\n", err)
		}
	}, nil
}

func printJSON(value any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func normalizeInterspersedFlags(args []string, flagsWithValue map[string]bool) []string {
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") || arg == "--" {
			positionals = append(positionals, arg)
			continue
		}
		nameValue := strings.TrimPrefix(arg, "--")
		name, _, hasValue := strings.Cut(nameValue, "=")
		if !flagsWithValue[name] {
			positionals = append(positionals, arg)
			continue
		}
		flags = append(flags, arg)
		if !hasValue && i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return append(flags, positionals...)
}
