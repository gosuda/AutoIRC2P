package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/gosuda/AutoIRC2P/internal/auth"
	"github.com/gosuda/AutoIRC2P/internal/config"
	"github.com/gosuda/AutoIRC2P/internal/irc"
	"github.com/gosuda/AutoIRC2P/internal/server"
	"github.com/gosuda/AutoIRC2P/internal/store"
	"github.com/gosuda/AutoIRC2P/internal/translate"
	"gosuda.org/portalite"
)

func main() {
	if err := execute(os.Args[1:]); err != nil {
		slog.Error("command failed", "error", err)
		os.Exit(1)
	}
}
func run() (err error) {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	key, err := config.Key(filepath.Join(cfg.DataDir, "application.key"))
	if err != nil {
		return err
	}
	db, queries, err := store.NewSQLite(ctx, filepath.Join(cfg.DataDir, "chat.sqlite"))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	accounts, err := auth.New(queries, key)
	if err != nil {
		return err
	}
	translator, err := translate.New(translate.Config{BaseURL: cfg.BaseURL, APIKey: cfg.APIKey, Models: cfg.Models, Interval: cfg.Interval, Cooldown: cfg.Cooldown, FailureThreshold: 3}, &http.Client{Timeout: 60 * time.Second}, queries)
	if err != nil {
		return err
	}
	var app *server.Server
	bridge, err := irc.New(irc.Config{ConfigPath: cfg.IVNPConfig, Server: cfg.IRCServer, Rooms: cfg.Rooms, IdleTimeout: cfg.IRCIdleTimeout, PongTimeout: cfg.IRCPongTimeout, MaxAccounts: cfg.IRCMaxAccounts, AccountIdleGrace: cfg.IRCAccountIdleGrace}, func(event irc.Event) { app.Event(ctx, event) })
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, bridge.Close()) }()
	listener, allowOrigin, err := openHTTPListener(ctx, cfg)
	if err != nil {
		return err
	}
	var workers sync.WaitGroup
	defer func() {
		stop()
		closeErr := listener.Close()
		if !errors.Is(closeErr, net.ErrClosed) {
			err = errors.Join(err, closeErr)
		}
		workers.Wait()
	}()
	if exposure, ok := listener.(*portalite.Exposure); ok {
		workers.Go(func() { logPortaliteUpdates(exposure) })
	}
	app = server.New(server.Config{AllowOrigin: allowOrigin, WebDir: cfg.WebDir, Rooms: cfg.Rooms, SecureCookies: cfg.SecureCookies, Security: server.SecurityConfig{
		TrustedProxies:    cfg.Security.TrustedProxies,
		DisableRateLimits: cfg.Security.DisableRateLimits,
		MaxWebSockets:     cfg.Security.MaxWebSockets, MaxWebSocketsPerIP: cfg.Security.MaxWebSocketsPerIP, MaxWebSocketsPerAccount: cfg.Security.MaxWebSocketsPerAccount,
		WSHandshakesPerMinute: cfg.Security.WSHandshakesPerMinute, CursorUpdatesPerMinute: cfg.Security.CursorUpdatesPerMinute,
		SendRequestsPerMinute: cfg.Security.SendRequestsPerMinute, SendRequestsPerIPMinute: cfg.Security.SendRequestsPerIPMinute, MaxPendingSends: cfg.Security.MaxPendingSends,
	}}, queries, accounts, translator, bridge)
	workers.Go(func() { app.Run(ctx) })
	workers.Go(func() { runRetention(ctx, queries, cfg.Retention) })
	httpServer := &http.Server{Addr: cfg.Listen, Handler: app.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: cfg.HTTPBodyTimeout, WriteTimeout: cfg.HTTPWriteTimeout, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 16384}
	serverError := make(chan error, 1)
	workers.Go(func() {
		slog.Info("HTTP listening", "address", listener.Addr().String(), "portalite", cfg.Portalite)
		serverError <- httpServer.Serve(listener)
	})
	defer func() {
		stop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownErr := httpServer.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			shutdownErr = errors.Join(shutdownErr, httpServer.Close())
		}
		err = errors.Join(err, shutdownErr)
	}()
	if !cfg.Offline {
		if err = bridge.Start(ctx); err != nil {
			return err
		}
		observer, err := accounts.Observer(ctx)
		if err != nil {
			return err
		}
		if cfg.ObserverNick != "" {
			observer.Nick = cfg.ObserverNick
			observer.Password = cfg.ObserverPassword
		}
		if err = bridge.ConnectObserver(ctx, observer); err != nil {
			return err
		}
	} else {
		app.Event(ctx, irc.Event{Kind: "status", State: "disconnected", Text: "IRC_OFFLINE is enabled"})
	}
	select {
	case <-ctx.Done():
	case err = <-serverError:
		stop()
	}
	if errors.Is(err, http.ErrServerClosed) || (ctx.Err() != nil && errors.Is(err, net.ErrClosed)) {
		err = nil
	}
	return err
}
