package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/urfave/cli/v3"

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
			Name:    "dns-resolver",
			Sources: cli.EnvVars("DNS_RESOLVER"),
			Usage:   "DNS Resolver",
			Value:   "8.8.8.8:53",
			Validator: func(s string) error {
				_, err := netip.ParseAddrPort(s)
				return err
			},
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
	},

	Action: func(ctx context.Context, cmd *cli.Command) error {
		t := sync.New(&sync.Config{
			NetbirdEndpoint:    cmd.String("netbird-endpoint"),
			NetbirdAccessToken: cmd.String("netbird-access-token"),
			DNSResolver:        cmd.String("dns-resolver"),
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
		_, err = s.NewJob(
			gocron.DurationJob(interval),
			gocron.NewTask(t.Run),
			gocron.WithStartAt(gocron.WithStartImmediately()),
		)
		if err != nil {
			return fmt.Errorf("new job: %w", err)
		}

		s.Start()

		done := make(chan os.Signal, 1)
		signal.Notify(done, os.Interrupt, syscall.SIGTERM)
		<-done
		return nil

	},
}

func main() {
	_ = rootCmd.Run(context.Background(), os.Args)
}
