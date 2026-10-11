// Command lite-model-router is a small OpenAI/Anthropic-compatible router that
// picks a backend by model name and falls back across an ordered chain.
package main

import (
	"context"
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

	"github.com/zhuanyi/lite-model-router/internal/config"
	"github.com/zhuanyi/lite-model-router/internal/proxy"
)

var version = "dev" // overridden with -ldflags "-X main.version=..."

func main() { os.Exit(run()) }

func run() int {
	var (
		cfgPath  = flag.String("config", "config.json", "path to the JSON config file")
		listen   = flag.String("listen", "", "listen address (overrides config)")
		quiet    = flag.Bool("quiet", false, "disable per-request log lines")
		showVers = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()
	if *showVers {
		fmt.Println("lite-model-router", version)
		return 0
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := config.Load(*cfgPath)
	if err == nil && *listen != "" {
		err = cfg.SetListen(*listen)
	}
	if err != nil {
		log.Error("invalid configuration", "err", err)
		return 2
	}
	if *quiet {
		cfg.LogRequests = false
	}
	for _, w := range cfg.Warnings {
		log.Warn(w)
	}

	h, err := proxy.New(cfg, log)
	if err != nil {
		log.Error("startup failed", "err", err)
		return 2
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Error("listen failed", "err", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	h.Start(ctx)

	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		// No WriteTimeout: responses stream.
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	log.Info("listening", "addr", ln.Addr().String(), "version", version,
		"backends", len(cfg.Backends), "models", len(cfg.Models))

	select {
	case err := <-serveErr:
		log.Error("server failed", "err", err)
		return 1
	case <-ctx.Done():
		stop() // restore default handling: a second signal kills us during the drain
	}

	log.Info("shutting down", "drain", "10s")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		if !errors.Is(err, context.DeadlineExceeded) {
			log.Error("shutdown", "err", err)
		}
		_ = srv.Close() // drain window over: cut remaining connections
	}
	h.Close()
	return 0
}
