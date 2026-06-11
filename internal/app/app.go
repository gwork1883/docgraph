package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/docgraph/docgraph/internal/config"
	"github.com/docgraph/docgraph/internal/mcp"
	"github.com/docgraph/docgraph/internal/query"
	"github.com/docgraph/docgraph/internal/server"
	"github.com/docgraph/docgraph/internal/storage"
)

func Init(ctx context.Context, cfg config.Config) error {
	store, err := storage.Open(ctx, cfg.Storage.DSN)
	if err != nil {
		return err
	}
	defer store.Close()

	return store.Migrate(ctx)
}

func Serve(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	store, err := storage.Open(ctx, cfg.Storage.DSN)
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.Migrate(ctx); err != nil {
		return err
	}

	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	srv := server.NewWithAuth(addr, store, logger, cfg.Auth)
	return srv.Run(ctx)
}

func Status(ctx context.Context, cfg config.Config) (storage.Status, error) {
	store, err := storage.Open(ctx, cfg.Storage.DSN)
	if err != nil {
		return storage.Status{}, err
	}
	defer store.Close()

	if err := store.Migrate(ctx); err != nil {
		return storage.Status{}, err
	}
	return store.Status(ctx)
}

func MCP(ctx context.Context, cfg config.Config, in io.Reader, out io.Writer) error {
	store, err := storage.Open(ctx, cfg.Storage.DSN)
	if err != nil {
		return err
	}
	defer store.Close()

	if err := store.Migrate(ctx); err != nil {
		return err
	}

	return mcp.NewServerWithStore(query.NewService(store), store, in, out).Run(ctx)
}
