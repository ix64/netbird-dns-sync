package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/dns"
	"github.com/cloudflare/cloudflare-go/v6/option"
	"github.com/cloudflare/cloudflare-go/v6/zones"

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
	slog.Info("netbird peers", slog.Int("count", len(resp)))

	if err := t.initZone(ctx); err != nil {
		return fmt.Errorf("init zone: %w", err)
	}
	slog.Info("zone ready", slog.String("domain", t.cfg.CloudflareDomain))

	for _, v := range resp {
		if err := t.syncRecord(ctx, v.DNSLabel, v.IP); err != nil {
			return fmt.Errorf("sync record(%s): %w", v.DNSLabel, err)
		}
	}
	slog.Info("sync completed", slog.Int("records", len(resp)))
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
	slog.Info("cloudflare zone initialized", slog.String("zone_id", t.cloudflareZoneID))
	return nil
}

func (t *Sync) createRecord(ctx context.Context, domain string, ipStr string) error {
	name, ok := strings.CutSuffix(domain, "."+t.cfg.CloudflareDomain)
	if !ok {
		return fmt.Errorf("invalid domain %s without suffix %s", domain, t.cfg.CloudflareDomain)
	}

	_, err := t.cloudflare.DNS.Records.New(ctx, dns.RecordNewParams{
		ZoneID: cloudflare.String(t.cloudflareZoneID),
		Body: dns.ARecordParam{
			Type:    cloudflare.F(dns.ARecordTypeA),
			Name:    cloudflare.String(name),
			Content: cloudflare.String(ipStr),
			Proxied: cloudflare.Bool(false),
		},
	})
	if err != nil {
		return fmt.Errorf("create record: %w", err)
	}

	return nil
}

func (t *Sync) syncRecord(ctx context.Context, domain string, ipStr string) error {
	if _, ok := strings.CutSuffix(domain, "."+t.cfg.CloudflareDomain); !ok {
		return fmt.Errorf("invalid domain %s without suffix %s", domain, t.cfg.CloudflareDomain)
	}

	list, err := t.cloudflare.DNS.Records.List(ctx, dns.RecordListParams{
		ZoneID: cloudflare.String(t.cloudflareZoneID),
		Name: cloudflare.F(dns.RecordListParamsName{
			Exact: cloudflare.F(domain),
		}),
		Type: cloudflare.F(dns.RecordListParamsTypeA),
	})
	if err != nil {
		return fmt.Errorf("list record(%s): %w", domain, err)
	}

	// create if empty
	if len(list.Result) == 0 {
		if err := t.createRecord(ctx, domain, ipStr); err != nil {
			return err
		}
		slog.Info("dns record setup", slog.String("domain", domain), slog.String("ip", ipStr))
		return nil
	}

	var keepID string
	for _, record := range list.Result {
		if record.Content == ipStr {
			keepID = record.ID
			break
		}
	}

	if keepID == "" {
		target := list.Result[0]
		if err := t.updateRecord(ctx, target.ID, domain, ipStr, target.Proxied); err != nil {
			return err
		}
		keepID = target.ID
		slog.Info("dns record updated", slog.String("domain", domain), slog.String("ip", ipStr))
	} else {
		slog.Info("dns record exist", slog.String("domain", domain), slog.String("ip", ipStr))
	}

	for _, record := range list.Result {
		if record.ID == keepID {
			continue
		}
		if err := t.deleteRecord(ctx, record.ID); err != nil {
			return err
		}
		slog.Info("dns record removed", slog.String("domain", domain), slog.String("ip", record.Content))
	}

	return nil
}

func (t *Sync) updateRecord(ctx context.Context, recordID string, domain string, ipStr string, proxied bool) error {
	_, err := t.cloudflare.DNS.Records.Update(ctx, recordID, dns.RecordUpdateParams{
		ZoneID: cloudflare.String(t.cloudflareZoneID),
		Body: dns.ARecordParam{
			Type:    cloudflare.F(dns.ARecordTypeA),
			Name:    cloudflare.String(domain),
			Content: cloudflare.String(ipStr),
			Proxied: cloudflare.Bool(proxied),
		},
	})
	if err != nil {
		return fmt.Errorf("update record(%s): %w", domain, err)
	}
	return nil
}

func (t *Sync) deleteRecord(ctx context.Context, recordID string) error {
	_, err := t.cloudflare.DNS.Records.Delete(ctx, recordID, dns.RecordDeleteParams{
		ZoneID: cloudflare.String(t.cloudflareZoneID),
	})
	if err != nil {
		return fmt.Errorf("delete record(%s): %w", recordID, err)
	}
	return nil
}
