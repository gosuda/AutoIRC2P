package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gosuda/AutoIRC2P/internal/config"
	"github.com/gosuda/AutoIRC2P/internal/store"
)

func execute(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Println("Usage: autoirc2p [serve | backup --out DIR [--data-dir DIR] | restore --from DIR --data-dir NEW_DIR]")
			return nil
		}
	}
	if err := config.LoadEnv(".env"); err != nil {
		return err
	}
	if len(args) == 0 {
		return run()
	}
	switch args[0] {
	case "serve":
		if len(args) != 1 {
			return fmt.Errorf("serve accepts no arguments")
		}
		return run()
	case "backup", "restore":
		return maintainCommand(args[0], args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func maintainCommand(command string, args []string) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	defaultData := cmp.Or(os.Getenv("DATA_DIR"), "data")
	if command == "restore" {
		defaultData = ""
	}
	dataDir := flags.String("data-dir", defaultData, "application data directory")
	timeout := flags.Duration("timeout", 10*time.Minute, "operation deadline")
	var source, target *string
	if command == "backup" {
		target = flags.String("out", "", "new backup directory")
	} else {
		source = flags.String("from", "", "backup directory")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *dataDir == "" || *timeout <= 0 {
		return fmt.Errorf("invalid %s arguments", command)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	if command == "backup" {
		if *target == "" {
			return fmt.Errorf("backup requires --out")
		}
		if err := store.Backup(ctx, filepath.Join(*dataDir, "chat.sqlite"), filepath.Join(*dataDir, "application.key"), *target); err != nil {
			return err
		}
		slog.Info("backup created", "directory", *target)
		return nil
	}
	if *source == "" {
		return fmt.Errorf("restore requires --from")
	}
	if err := store.Restore(ctx, *source, *dataDir); err != nil {
		return err
	}
	slog.Info("backup restored", "directory", *dataDir)
	return nil
}

func runRetention(ctx context.Context, q *store.Queries, cfg config.Retention) {
	policy := store.RetentionPolicy{Messages: cfg.Messages, Translations: cfg.Translations, SendPayloads: cfg.SendPayloads, BatchSize: cfg.BatchSize}
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		pruneCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		result, err := q.Prune(pruneCtx, time.Now(), policy)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("retention incomplete", "error", err)
		}
		if result.MessagesDeleted+result.TranslationsDeleted+result.SendPayloadsPurged > 0 {
			slog.Info("retention completed", "messages", result.MessagesDeleted, "translations", result.TranslationsDeleted, "send_payloads", result.SendPayloadsPurged)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
