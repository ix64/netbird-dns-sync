package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/google/uuid"
	"github.com/urfave/cli/v3"

	_ "golang.org/x/crypto/x509roots/fallback"

	"github.com/ix64/netbird-dns-sync/internal/sync"
)

var rootCmd = &cli.Command{
	Description: "Sync Netbird Internal DNS Record to Cloudflare Zone",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:     "netbird-endpoint",
			Sources:  cli.EnvVars("NETBIRD_ENDPOINT"),
			Required: true,
			Usage:    "Netbird Management Endpoint (e.g. https://netbird.example.com)",
		},
		&cli.StringFlag{
			Name:     "netbird-access-token",
			Sources:  cli.EnvVars("NETBIRD_ACCESS_TOKEN"),
			Required: true,
			Usage:    "Netbird Access Token (Required)",
		},

		&cli.StringFlag{
			Name:     "cloudflare-api-token",
			Sources:  cli.EnvVars("CLOUDFLARE_API_TOKEN"),
			Required: true,
			Usage:    "Cloudflare API Token (Required)",
		},
		&cli.StringFlag{
			Name:     "cloudflare-domain",
			Sources:  cli.EnvVars("CLOUDFLARE_DOMAIN"),
			Required: true,
			Usage:    "Cloudflare Zone Domain (Required)",
		},
		&cli.StringFlag{
			Name:    "sync-interval",
			Sources: cli.EnvVars("SYNC_INTERVAL"),
			Usage:   "Sync Interval",
			Value:   "15m",
			Validator: func(s string) error {
				v, err := time.ParseDuration(s)
				if err != nil {
					return err
				}
				if v < time.Second {
					return errors.New("sync-interval must be at least 1 seconds")
				}
				return nil
			},
		},
		&cli.StringFlag{
			Name:    "log-level",
			Sources: cli.EnvVars("LOG_LEVEL"),
			Usage:   "Log Level (debug, info, warn, error)",
			Value:   "info",
			Validator: func(s string) error {
				var l slog.Level
				return l.UnmarshalText([]byte(s))
			},
		},
		&cli.StringFlag{
			Name:    "log-format",
			Sources: cli.EnvVars("LOG_FORMAT"),
			Usage:   "Log Format (text, json)",
			Value:   "text",
			Validator: func(s string) error {
				if s != "text" && s != "json" {
					return errors.New("log-format must be text or json")
				}
				return nil
			},
		},
	},

	Action: func(ctx context.Context, cmd *cli.Command) error {
		var level slog.Level
		if err := level.UnmarshalText([]byte(cmd.String("log-level"))); err != nil {
			return fmt.Errorf("parse log-level: %w", err)
		}
		slog.SetDefault(newLogger(cmd.String("log-format"), level))

		t := sync.New(&sync.Config{
			NetbirdEndpoint:    cmd.String("netbird-endpoint"),
			NetbirdAccessToken: cmd.String("netbird-access-token"),
			CloudflareAPIToken: cmd.String("cloudflare-api-token"),
			CloudflareDomain:   cmd.String("cloudflare-domain"),
		})

		s, err := gocron.NewScheduler()
		if err != nil {
			return fmt.Errorf("new scheduler: %w", err)
		}

		interval, err := time.ParseDuration(cmd.String("sync-interval"))
		if err != nil {
			return fmt.Errorf("parse sync-interval: %w", err)
		}
		slog.Info("starting",
			slog.String("netbird_endpoint", cmd.String("netbird-endpoint")),
			slog.String("zone", cmd.String("cloudflare-domain")),
			slog.String("interval", interval.String()),
			slog.String("log_level", level.String()),
		)

		_, err = s.NewJob(
			gocron.DurationJob(interval),
			gocron.NewTask(t.Run),
			gocron.WithStartAt(gocron.WithStartImmediately()),
			gocron.WithSingletonMode(gocron.LimitModeReschedule),
			gocron.WithEventListeners(
				gocron.AfterJobRunsWithError(func(_ uuid.UUID, _ string, err error) {
					slog.Error("sync failed", slog.Any("error", err))
				}),
			),
		)
		if err != nil {
			return fmt.Errorf("new job: %w", err)
		}

		s.Start()

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		<-ctx.Done()

		slog.Info("shutting down")
		if err := s.Shutdown(); err != nil {
			return fmt.Errorf("shutdown scheduler: %w", err)
		}
		return nil
	},
}

func newLogger(format string, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level: level,
		// API errors may carry trailing newlines; keep each entry on one line
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if err, ok := a.Value.Any().(error); ok {
				return slog.String(a.Key, strings.TrimSpace(err.Error()))
			}
			return a
		},
	}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func main() {
	if err := rootCmd.Run(context.Background(), os.Args); err != nil {
		slog.Error("exit with error", slog.Any("error", err))
		os.Exit(1)
	}
}
