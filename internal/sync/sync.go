package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/dns"
	"github.com/cloudflare/cloudflare-go/v4/option"
	"github.com/cloudflare/cloudflare-go/v4/zones"

	"github.com/ix64/netbird-dns-sync/internal/netbird"
)

type Config struct {
	NetbirdEndpoint    string
	NetbirdAccessToken string

	DNSResolver string

	// TODO: multiple dns resolver support
	CloudflareAPIToken string
	CloudflareDomain   string
}
type Sync struct {
	http             *http.Client
	resolver         *net.Resolver
	cfg              *Config
	cloudflare       *cloudflare.Client
	cloudflareZoneID string
}

func New(cfg *Config) *Sync {
	ret := &Sync{
		http:     &http.Client{},
		resolver: net.DefaultResolver,
		cloudflare: cloudflare.NewClient(
			option.WithAPIToken(cfg.CloudflareAPIToken),
		),
		cfg: cfg,
	}
	if cfg.DNSResolver != "" {
		ret.resolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{Timeout: time.Second}
				return d.DialContext(ctx, "udp", cfg.DNSResolver)
			},
		}

	}
	// TODO: allow using different dns

	return ret
}

func (t *Sync) Run(ctx context.Context) error {

	resp, err := t.getPeers(ctx)
	if err != nil {
		return fmt.Errorf("get peers: %w", err)
	}

	if err := t.initZone(ctx); err != nil {
		return fmt.Errorf("init zone: %w", err)
	}

	for _, v := range resp {
		ok, err := t.assertRecord(ctx, v.DNSLabel, v.IP)
		if err != nil {
			return fmt.Errorf("assert domain(%s): %w", v.DNSLabel, err)
		}
		if ok {
			slog.Info("dns record exist", slog.String("domain", v.DNSLabel), slog.String("ip", v.IP))
			continue
		}

		if err := t.setupRecord(ctx, v.DNSLabel, v.IP); err != nil {
			return fmt.Errorf("setup record(%s): %w", v.DNSLabel, err)
		}
		slog.Info("dns record setup", slog.String("domain", v.DNSLabel), slog.String("ip", v.IP))
	}
	return nil
}

func (t *Sync) getPeers(ctx context.Context) ([]netbird.Peer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.cfg.NetbirdEndpoint+"/api/peers", nil)
	if err != nil {
		return nil, fmt.Errorf("http create request: %w", err)
	}

	req.Header.Add("Accept", "application/json")
	req.Header.Add("Authorization", "Token "+t.cfg.NetbirdAccessToken)

	resp, err := t.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http get: %d", resp.StatusCode)
	}

	var respBody []netbird.Peer
	if err := json.NewDecoder(resp.Body).Decode(&respBody); err != nil {
		return nil, fmt.Errorf("json unmarshal: %w", err)
	}

	return respBody, nil
}

func (t *Sync) assertRecord(ctx context.Context, domain string, ipStr string) (bool, error) {

	ip, err := netip.ParseAddr(ipStr)
	if err != nil {
		return false, fmt.Errorf("parse ip: %w", err)
	}

	ips, err := t.resolver.LookupNetIP(ctx, "ip4", domain)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return false, nil
		}
		return false, fmt.Errorf("lookup %s: %w", domain, err)
	}

	for _, v := range ips {
		slog.Debug("exist dns record", slog.String("domain", domain), slog.String("ip", v.String()))
	}

	return slices.ContainsFunc(ips, func(v netip.Addr) bool { return ip.Compare(v) == 0 }), nil
}

func (t *Sync) initZone(ctx context.Context) error {
	ret, err := t.cloudflare.Zones.List(ctx, zones.ZoneListParams{
		Name: cloudflare.String(t.cfg.CloudflareDomain),
	})
	if err != nil {
		return fmt.Errorf("list zones: %w", err)
	}

	if len(ret.Result) == 0 {
		return fmt.Errorf("zone %s not found", cloudflare.String(t.cfg.CloudflareDomain))
	}

	t.cloudflareZoneID = ret.Result[0].ID
	return nil
}

func (t *Sync) setupRecord(ctx context.Context, domain string, ipStr string) error {
	// TODO: remove incorrect record
	name, ok := strings.CutSuffix(domain, "."+t.cfg.CloudflareDomain)
	if !ok {
		return fmt.Errorf("invalid domain %s without suffix %s", domain, t.cfg.CloudflareDomain)
	}

	_, err := t.cloudflare.DNS.Records.New(ctx, dns.RecordNewParams{
		ZoneID: cloudflare.String(t.cloudflareZoneID),
		Record: dns.ARecordParam{
			Type:    cloudflare.F(dns.ARecordTypeA),
			Name:    cloudflare.String(name),
			Content: cloudflare.String(ipStr),
			Proxied: cloudflare.Bool(false),
		},
	})
	if err != nil {
		return fmt.Errorf("update dns record: %w", err)
	}

	return nil
}
