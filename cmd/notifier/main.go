package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/becreacom/flyio-slack/internal/config"
	"github.com/becreacom/flyio-slack/internal/digest"
	"github.com/becreacom/flyio-slack/internal/event"
	"github.com/becreacom/flyio-slack/internal/flyapi"
	"github.com/becreacom/flyio-slack/internal/poller"
	"github.com/becreacom/flyio-slack/internal/slack"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config.yaml")
	envPath := flag.String("env", ".env", "path to .env (optional)")
	verbose := flag.Bool("v", false, "verbose logging")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if err := config.LoadDotenv(*envPath); err != nil {
		logger.Warn("failed loading .env (continuing)", "path", *envPath, "err", err)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("config load failed", "err", err)
		os.Exit(1)
	}

	store, err := poller.OpenStore(cfg.StateFile)
	if err != nil {
		logger.Error("open state store failed", "err", err, "path", cfg.StateFile)
		os.Exit(1)
	}
	defer store.Close()

	client := flyapi.New(cfg.Fly.BaseURL, cfg.Fly.APIToken)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	events := make(chan event.Event, 256)

	apps := make([]string, 0, len(cfg.Apps))
	for _, a := range cfg.Apps {
		apps = append(apps, a.Name)
	}

	dispatcher := slack.NewDispatcher(cfg.Slack.DefaultWebhook, cfg.DedupWindow.Get(), logger.With("component", "slack"))

	p := poller.New(client, apps, cfg.PollInterval.Get(), store, events, logger.With("component", "poller"))

	dig := &digest.Digester{
		Client: client,
		Apps:   apps,
		Out:    events,
		Logger: logger.With("component", "digest"),
	}

	var c *cron.Cron
	if cfg.Digest.Enabled {
		c = cron.New(cron.WithLocation(time.UTC))
		_, err := c.AddFunc(cfg.Digest.Schedule, func() {
			dig.Run(ctx)
		})
		if err != nil {
			logger.Error("invalid digest schedule", "schedule", cfg.Digest.Schedule, "err", err)
			os.Exit(1)
		}
		c.Start()
		defer c.Stop()
		logger.Info("digest scheduled", "schedule", cfg.Digest.Schedule)
	}

	go dispatcher.Run(ctx, events)

	logger.Info("notifier starting",
		"apps", apps,
		"poll_interval", cfg.PollInterval.Get(),
		"dedup_window", cfg.DedupWindow.Get(),
		"state_file", cfg.StateFile,
	)

	if err := p.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("poller exited unexpectedly", "err", err)
		os.Exit(1)
	}

	logger.Info("notifier stopped")
}
