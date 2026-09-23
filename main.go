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
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	configPath := flag.String("config", "/etc/tuwuyeet/config.yaml", "path to config file")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: tuwuyeet [-config path] [run prune-rooms|expire-media]\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fatal("config", err)
	}
	reg, err := loadRegistration(cfg.Registration)
	if err != nil {
		fatal("registration", err)
	}
	client := NewClient(cfg.Homeserver, cfg.ServerName, reg)
	jobs := buildJobs(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if args := flag.Args(); len(args) > 0 {
		if len(args) != 2 || args[0] != "run" {
			flag.Usage()
			os.Exit(2)
		}
		os.Exit(runOnce(ctx, cfg, client, jobs, args[1]))
	}

	cpu := LoadCPU(filepath.Join(cfg.DataDir, "cpu.json"))
	sched := NewScheduler(cfg, client, cpu, jobs, filepath.Join(cfg.DataDir, "state.json"))

	go sampleCPU(ctx, cpu, "/proc/stat", time.Minute)
	go bootstrap(ctx, cfg, client)
	go sched.Loop(ctx)

	srv := &http.Server{Addr: cfg.Listen, Handler: serveWeb(sched, cpu, cfg.loc), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("tuwuyeet started", "listen", cfg.Listen, "homeserver", cfg.Homeserver, "bot", client.userID)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal("http", err)
	}
	if err := cpu.Save(); err != nil {
		slog.Warn("saving cpu history failed", "err", err)
	}
	slog.Info("tuwuyeet stopped")
}

func runOnce(ctx context.Context, cfg Config, client *Client, jobs []*Job, name string) int {
	for _, j := range jobs {
		if j.Name == name {
			if _, err := RunJob(ctx, cfg, client, j); err != nil {
				return 1
			}
			return 0
		}
	}
	fmt.Fprintf(os.Stderr, "unknown job %q (want prune-rooms or expire-media)\n", name)
	return 2
}

func bootstrap(ctx context.Context, cfg Config, client *Client) {
	delay := 5 * time.Second
	for {
		err := client.EnsureRegistered(ctx)
		if err == nil {
			break
		}
		slog.Warn("bot registration failed, retrying", "err", err, "in", delay.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Minute)
	}
	slog.Info("bot registered", "bot", client.userID)
	roomID, err := client.ResolveAlias(ctx, "#admins:"+cfg.ServerName)
	if err != nil {
		slog.Error("admin room not found", "err", err)
		return
	}
	joined, err := client.IsJoined(ctx, roomID)
	switch {
	case err != nil:
		slog.Error("checking admin room membership failed", "err", err)
	case !joined:
		slog.Error("bot is not in the admin room", "room", roomID, "err", ErrNotAdmin)
	default:
		slog.Info("admin room ready", "room", roomID)
	}
}

func fatal(what string, err error) {
	slog.Error("fatal", "stage", what, "err", err)
	os.Exit(1)
}
