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
	"github.com/docgraph/docgraph/internal/mcp"
	"github.com/docgraph/docgraph/internal/query"
	"github.com/docgraph/docgraph/internal/storage"
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
		return runInit(args[2:])
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
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[1])
	}
}

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
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

	return app.Init(context.Background(), cfg)
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file path")
	host := fs.String("host", "", "server host")
	port := fs.Int("port", 0, "server port")
	dataDir := fs.String("data", "", "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return app.Serve(ctx, cfg, logger)
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
			return fmt.Errorf("database is not initialized; run docgraph init")
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

	store, err := storage.Open(ctx, cfg.Storage.DSN)
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.Migrate(ctx); err != nil {
		return err
	}

	return mcp.NewServerWithStore(query.NewService(store), store, os.Stdin, os.Stdout).Run(ctx)
}

func printUsage() {
	fmt.Print(`DocGraph

Usage:
  docgraph version
  docgraph init [--config docgraph.yaml] [--data ./.docgraph]
  docgraph serve [--config docgraph.yaml] [--host 127.0.0.1] [--port 8787] [--data ./.docgraph]
  docgraph status [--config docgraph.yaml] [--data ./.docgraph]
  docgraph mcp [--config docgraph.yaml] [--data ./.docgraph]
  docgraph source add --name "Docs" --dsn /path/to/docs [--data ./.docgraph]
  docgraph source list [--data ./.docgraph]
  docgraph source update --id src_xxx [--name "Docs"] [--dsn /path/to/docs] [--product Product] [--module Module] [--data ./.docgraph]
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
`)
}
