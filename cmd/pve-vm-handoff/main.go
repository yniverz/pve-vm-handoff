// Command pve-vm-handoff serves a small web UI that hands shared hardware
// (typically a passthrough GPU) between Proxmox VMs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yniverz/pve-vm-handoff/internal/config"
	"github.com/yniverz/pve-vm-handoff/internal/engine"
	"github.com/yniverz/pve-vm-handoff/internal/proxmox"
	"github.com/yniverz/pve-vm-handoff/internal/web"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}

	path := flag.String("config", envOr("CONFIG_FILE", "/config/config.yaml"), "path to config.yaml (CONFIG_YAML overrides it with inline YAML)")
	check := flag.Bool("check", false, "validate the config and exit")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(log)

	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:\n  "+strings.ReplaceAll(err.Error(), "\n", "\n  "))
		os.Exit(2)
	}
	if *check {
		fmt.Printf("config ok: %d host(s), %d vm(s), %d group(s)\n", len(cfg.Hosts), len(cfg.VMs), len(cfg.Groups))
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	apis := map[string]proxmox.API{}
	for name, h := range cfg.Hosts {
		apis[name] = proxmox.New(name, h)
	}
	eng := engine.New(ctx, cfg, apis, engine.Options{Logger: log})
	go eng.Run(ctx)
	go engine.Notify(ctx, eng.Events(), cfg.Notifications, log)

	srv := &http.Server{Addr: cfg.Server.Listen, Handler: web.New(eng, cfg.Server.UserHeader), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	log.Info("listening", "addr", cfg.Server.Listen, "hosts", len(cfg.Hosts), "vms", len(cfg.VMs), "groups", len(cfg.Groups))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func healthcheck() int {
	port := envOr("HEALTHCHECK_PORT", "8080")
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
