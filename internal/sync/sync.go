package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/dns"
	"github.com/cloudflare/cloudflare-go/v6/option"
	"github.com/cloudflare/cloudflare-go/v6/zones"

	"github.com/ix64/netbird-dns-sync/internal/netbird"
)

// managedComment marks DNS records owned by netbird-dns-sync.
// Only records carrying it are removed once their peer is gone.
const managedComment = "managed-by: netbird-dns-sync"

type Config struct {
	NetbirdEndpoint    string
	NetbirdAccessToken string

	CloudflareAPIToken string
	CloudflareDomain   string
}
type Sync struct {
	http             *http.Client
	cfg              *Config
	cloudflare       *cloudflare.Client
	cloudflareZoneID string
}

func New(cfg *Config) *Sync {
	ret := &Sync{
		http: &http.Client{Timeout: 30 * time.Second},
		cloudflare: cloudflare.NewClient(
			option.WithAPIToken(cfg.CloudflareAPIToken),
		),
		cfg: cfg,
	}

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

	var errs []error
	var synced, skipped int
	keep := make(map[string]struct{}, len(resp))
	for _, v := range resp {
		if _, ok := t.recordName(v.DNSLabel); !ok || v.IP == "" {
			slog.Warn("peer skipped",
				slog.String("dns_label", v.DNSLabel),
				slog.String("ip", v.IP),
				slog.String("reason", "dns label not under zone or ip empty"),
			)
			skipped++
			continue
		}
		keep[strings.ToLower(v.DNSLabel)] = struct{}{}
		if err := t.syncRecord(ctx, v.DNSLabel, v.IP); err != nil {
			slog.Error("sync record failed", slog.String("domain", v.DNSLabel), slog.Any("error", err))
			errs = append(errs, fmt.Errorf("sync record(%s): %w", v.DNSLabel, err))
			continue
		}
		synced++
	}

	removed, err := t.cleanupRecords(ctx, keep)
	if err != nil {
		errs = append(errs, fmt.Errorf("cleanup records: %w", err))
	}

	slog.Info("sync completed",
		slog.Int("synced", synced),
		slog.Int("skipped", skipped),
		slog.Int("removed", removed),
		slog.Int("failed", len(errs)),
	)
	return errors.Join(errs...)
}

// recordName returns the record name relative to the zone,
// or false if domain is not under the zone.
func (t *Sync) recordName(domain string) (string, bool) {
	name, ok := strings.CutSuffix(domain, "."+t.cfg.CloudflareDomain)
	if !ok || name == "" {
		return "", false
	}
	return name, true
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
		return fmt.Errorf("zone %s not found", t.cfg.CloudflareDomain)
	}

	t.cloudflareZoneID = ret.Result[0].ID
	slog.Info("cloudflare zone initialized", slog.String("zone_id", t.cloudflareZoneID))
	return nil
}

func (t *Sync) createRecord(ctx context.Context, domain string, ipStr string) error {
	name, ok := t.recordName(domain)
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
			Comment: cloudflare.String(managedComment),
		},
	})
	if err != nil {
		return fmt.Errorf("create record: %w", err)
	}

	return nil
}

func (t *Sync) syncRecord(ctx context.Context, domain string, ipStr string) error {
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

	keepIdx := -1
	for i, record := range list.Result {
		if record.Content == ipStr {
			keepIdx = i
			break
		}
	}

	var keepID string
	switch {
	case keepIdx < 0:
		target := list.Result[0]
		if err := t.updateRecord(ctx, target.ID, domain, ipStr, target.Proxied); err != nil {
			return err
		}
		keepID = target.ID
		slog.Info("dns record updated", slog.String("domain", domain), slog.String("ip", ipStr))
	case list.Result[keepIdx].Comment != managedComment:
		// adopt records created before ownership marking, so they can be cleaned up later
		target := list.Result[keepIdx]
		if err := t.updateRecord(ctx, target.ID, domain, ipStr, target.Proxied); err != nil {
			return err
		}
		keepID = target.ID
		slog.Info("dns record adopted", slog.String("domain", domain), slog.String("ip", ipStr))
	default:
		keepID = list.Result[keepIdx].ID
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
			Comment: cloudflare.String(managedComment),
		},
	})
	if err != nil {
		return fmt.Errorf("update record(%s): %w", domain, err)
	}
	return nil
}

// cleanupRecords removes managed A records whose domain is not in keep,
// returning the number of records removed.
func (t *Sync) cleanupRecords(ctx context.Context, keep map[string]struct{}) (int, error) {
	iter := t.cloudflare.DNS.Records.ListAutoPaging(ctx, dns.RecordListParams{
		ZoneID: cloudflare.String(t.cloudflareZoneID),
		Type:   cloudflare.F(dns.RecordListParamsTypeA),
		Comment: cloudflare.F(dns.RecordListParamsComment{
			Exact: cloudflare.String(managedComment),
		}),
	})

	// collect first: deleting while paging would shift later pages
	var stale []dns.RecordResponse
	for iter.Next() {
		record := iter.Current()
		// the comment filter is case-insensitive; only delete exact matches
		if record.Comment != managedComment {
			continue
		}
		if _, ok := keep[strings.ToLower(record.Name)]; ok {
			continue
		}
		stale = append(stale, record)
	}
	if err := iter.Err(); err != nil {
		return 0, fmt.Errorf("list managed records: %w", err)
	}

	var errs []error
	var removed int
	for _, record := range stale {
		if err := t.deleteRecord(ctx, record.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		slog.Info("stale dns record removed", slog.String("domain", record.Name), slog.String("ip", record.Content))
		removed++
	}
	return removed, errors.Join(errs...)
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
