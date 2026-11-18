// Command teams-a2a-gateway connects A2A agents to Microsoft Teams through Azure Bot Service.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/codentient/teams-a2a/internal/config"
	"github.com/codentient/teams-a2a/internal/gateway"
	"github.com/codentient/teams-a2a/internal/metrics"
	"github.com/codentient/teams-a2a/internal/store"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/teams-a2a/config.yaml", "path to the configuration file")
	validateOnly := flag.Bool("validate", false, "validate the configuration and exit")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *validateOnly {
		fmt.Println("configuration is valid")
		return nil
	}

	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.Server.LogLevel))
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	var st store.Store
	switch cfg.Store.Type {
	case "redis":
		password := ""
		if cfg.Store.Redis.Password.IsSet() {
			if password, err = cfg.Store.Redis.Password.Resolve(); err != nil {
				return fmt.Errorf("redis password: %w", err)
			}
		}
		st = store.NewRedis(store.RedisOptions{
			Address: cfg.Store.Redis.Address, Username: cfg.Store.Redis.Username, Password: password,
			DB: cfg.Store.Redis.DB, TLS: cfg.Store.Redis.TLS, KeyPrefix: cfg.Store.Redis.KeyPrefix,
			SessionTTL: cfg.Store.SessionTTL.Duration,
		})
	default:
		st = store.NewMemory(cfg.Store.SessionTTL.Duration, cfg.Store.MaxSessions)
	}
	defer st.Close()

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute, // agents may take a while before the first byte
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	httpClient := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	// A2A streams can legitimately stay open for the whole turn, so no overall timeout here;
	// every call is bounded by the turn context instead.
	agentClient := &http.Client{Transport: transport}

	m := metrics.New()
	srv, err := gateway.New(cfg, gateway.Deps{Store: st, Metrics: m, Logger: log, HTTPClient: httpClient, AgentHTTPClient: agentClient})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	public := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	admin := &http.Server{Addr: cfg.Server.AdminListen, Handler: srv.AdminHandler(), ReadHeaderTimeout: 5 * time.Second}

	errc := make(chan error, 2)
	// The admin server starts first so probes see "alive but not ready" during warmup.
	go func() {
		if err := admin.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	// Signing keys are required before any request can be authenticated. Retry transient
	// failures (e.g. an IdP blip during a rollout) instead of crash-looping.
	backoff := time.Second
	deadline := time.Now().Add(5 * time.Minute)
	for {
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = srv.Warmup(wctx)
		cancel()
		if err == nil {
			break
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return fmt.Errorf("startup: %w", err)
		}
		log.Warn("warmup failed, retrying", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}

	// Bind before reporting ready, so /readyz never passes while the public port refuses connections.
	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Server.Listen, err)
	}
	go func() {
		var err error
		if cfg.Server.TLSCertFile != "" {
			err = public.ServeTLS(ln, cfg.Server.TLSCertFile, cfg.Server.TLSKeyFile)
		} else {
			err = public.Serve(ln)
		}
		if !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	srv.SetReady()
	log.Info("listening", "addr", ln.Addr().String(), "version", version, "routes", len(cfg.Routes), "store", cfg.Store.Type)
	if len(cfg.Routes) == 0 {
		log.Warn("no routes configured: every Bot Service request is rejected until a route is added")
	}

	select {
	case <-ctx.Done():
		// Report not-ready first and keep serving while load balancers drop the pod, so no
		// request reaches a closed port during a rollout.
		srv.SetNotReady()
		log.Info("shutting down", "drainDelay", cfg.Server.DrainDelay.Duration)
		time.Sleep(cfg.Server.DrainDelay.Duration)
	case err := <-errc:
		return err
	}
	sctx, scancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.Duration)
	defer scancel()
	_ = public.Shutdown(sctx)
	srv.Shutdown(sctx)
	_ = admin.Shutdown(sctx)
	return nil
}
