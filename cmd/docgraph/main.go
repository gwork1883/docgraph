package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/docgraph/docgraph/internal/app"
	"github.com/docgraph/docgraph/internal/config"
)

var version = "dev"

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "docgraph: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 {
		printUsage()
		return nil
	}

	switch args[1] {
	case "version":
		fmt.Println(version)
		return nil
	case "init":
		return runMigrate(args[2:])
	case "migrate":
		return runMigrate(args[2:])
	case "serve":
		return runServe(args[2:])
	case "status":
		return runStatus(args[2:])
	case "mcp":
		return runMCP(args[2:])
	case "source":
		return runSource(args[2:])
	case "search":
		return runSearch(args[2:])
	case "context":
		return runContext(args[2:])
	case "node":
		return runNode(args[2:])
	case "impact":
		return runImpact(args[2:])
	case "feedback":
		return runFeedback(args[2:])
	case "maintenance":
		return runMaintenance(args[2:])
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[1])
	}
}

func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	dataDir := fs.String("data", "", "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, _, err := loadOrCreateDefaultConfig(*cfgPath)
	if err != nil {
		return err
	}
	if *dataDir != "" {
		cfg.Server.DataDir = *dataDir
		cfg.Storage.DSN = "sqlite://" + filepath.ToSlash(filepath.Join(*dataDir, "docgraph.db"))
	}

	return app.Migrate(context.Background(), cfg)
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	host := fs.String("host", "", "server host")
	port := fs.Int("port", 0, "server port")
	dataDir := fs.String("data", "", "data directory")
	jobWorkers := fs.Int("job-workers", 0, "number of concurrent background job workers")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, configPath, err := loadOrCreateDefaultConfig(*cfgPath)
	if err != nil {
		return err
	}
	if *host != "" {
		cfg.Server.Host = *host
	}
	if *port != 0 {
		cfg.Server.Port = *port
	}
	if *dataDir != "" {
		cfg.Server.DataDir = *dataDir
		cfg.Storage.DSN = "sqlite://" + filepath.ToSlash(filepath.Join(*dataDir, "docgraph.db"))
	}
	if *jobWorkers != 0 {
		cfg.Server.JobWorkers = *jobWorkers
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if configPath != "" && *cfgPath == "" {
		logger.Info("using config", "path", configPath)
	}
	return app.Serve(ctx, cfg, logger)
}

func loadOrCreateDefaultConfig(explicitPath string) (config.Config, string, error) {
	if explicitPath != "" {
		cfg, err := config.Load(explicitPath)
		return cfg, explicitPath, err
	}

	if _, err := os.Stat(config.DefaultPath); err == nil {
		cfg, err := config.Load(config.DefaultPath)
		return cfg, config.DefaultPath, err
	} else if !errors.Is(err, os.ErrNotExist) {
		return config.Config{}, "", err
	}

	cfg, err := config.DefaultWithTokenAuth()
	if err != nil {
		return config.Config{}, "", err
	}
	if err := config.Write(config.DefaultPath, cfg); err != nil {
		return config.Config{}, "", err
	}
	return cfg, config.DefaultPath, nil
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	dataDir := fs.String("data", "", "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *dataDir != "" {
		cfg.Server.DataDir = *dataDir
		cfg.Storage.DSN = "sqlite://" + filepath.ToSlash(filepath.Join(*dataDir, "docgraph.db"))
	}

	status, err := app.Status(context.Background(), cfg)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("database is not initialized; run docgraph migrate")
		}
		return err
	}

	fmt.Printf("storage: %s\n", status.StorageDSN)
	fmt.Printf("sources: %d\n", status.Sources)
	fmt.Printf("documents: %d\n", status.Documents)
	fmt.Printf("sections: %d\n", status.Sections)
	fmt.Printf("nodes: %d\n", status.Nodes)
	fmt.Printf("edges: %d\n", status.Edges)
	fmt.Printf("jobs: %d\n", status.Jobs)
	return nil
}

func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	dataDir := fs.String("data", "", "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *dataDir != "" {
		cfg.Server.DataDir = *dataDir
		cfg.Storage.DSN = "sqlite://" + filepath.ToSlash(filepath.Join(*dataDir, "docgraph.db"))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return app.MCP(ctx, cfg, os.Stdin, os.Stdout)
}

func printUsage() {
	fmt.Print(`DocGraph

Usage:
  docgraph version
  docgraph migrate [--config docgraph.yaml] [--data ./.docgraph]
  docgraph init [--config docgraph.yaml] [--data ./.docgraph] (alias for migrate)
  docgraph serve [--config docgraph.yaml] [--host 127.0.0.1] [--port 8787] [--data ./.docgraph] [--job-workers 2]
  docgraph status [--config docgraph.yaml] [--data ./.docgraph]
  docgraph mcp [--config docgraph.yaml] [--data ./.docgraph]
  docgraph source add --name "Docs" --dsn /path/to/docs [--sync-schedule hourly] [--data ./.docgraph]
  docgraph source list [--data ./.docgraph]
  docgraph source update --id src_xxx [--name "Docs"] [--dsn /path/to/docs] [--product Product] [--module Module] [--sync-schedule manual] [--data ./.docgraph]
  docgraph source delete --id src_xxx [--data ./.docgraph]
  docgraph source sync --id src_xxx [--data ./.docgraph]
  docgraph source jobs --id src_xxx [--limit 20] [--data ./.docgraph]
  docgraph search [--data ./.docgraph] "member benefits"
  docgraph context [--data ./.docgraph] "Summarize member benefits"
  docgraph node get --id node_xxx [--data ./.docgraph]
  docgraph node related --id node_xxx [--direction both|out|in] [--kind contains] [--limit 20] [--data ./.docgraph]
  docgraph impact --id node_xxx [--direction out|in|both] [--kind exposes_api] [--max-depth 2] [--limit 50] [--data ./.docgraph]
  docgraph feedback add --target-kind edge --target-id edge_xxx --kind relationship_wrong [--payload '{}'] [--actor alice] [--data ./.docgraph]
  docgraph feedback list [--target-kind edge] [--target-id edge_xxx] [--kind relationship_wrong] [--limit 20] [--data ./.docgraph]
  docgraph maintenance section-entities-backfill [--source-id src_xxx|--document-id doc_xxx] [--data ./.docgraph]
`)
}
