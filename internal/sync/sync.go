package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
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

// syncResult is what syncing a peer's record changed.
type syncResult int

const (
	resultUnchanged syncResult = iota
	resultCreated
	resultUpdated
)

const (
	typeA    = "A"
	typeAAAA = "AAAA"
)

// recordKey identifies a record set by type and lowercased name.
type recordKey struct {
	typ  string
	name string
}

// peerRecord is one address record a peer should have.
type peerRecord struct {
	typ string
	ip  string
}

func peerRecords(p netbird.Peer) []peerRecord {
	var out []peerRecord
	if p.IP != "" {
		out = append(out, peerRecord{typ: typeA, ip: p.IP})
	}
	if p.IPv6 != "" {
		out = append(out, peerRecord{typ: typeAAAA, ip: p.IPv6})
	}
	return out
}

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
	start := time.Now()

	resp, err := t.getPeers(ctx)
	if err != nil {
		return fmt.Errorf("get peers: %w", err)
	}
	slog.Debug("netbird peers fetched", slog.Int("count", len(resp)))

	if err := t.initZone(ctx); err != nil {
		return fmt.Errorf("init zone: %w", err)
	}

	var created, updated, unchanged, skipped, failed int
	keep := make(map[recordKey]struct{}, len(resp))
	for _, v := range resp {
		if reason := t.skipReason(v); reason != "" {
			slog.Debug("peer skipped",
				slog.String("record", v.DNSLabel),
				slog.String("ip", v.IP),
				slog.String("ipv6", v.IPv6),
				slog.String("reason", reason),
			)
			skipped++
			continue
		}
		for _, r := range peerRecords(v) {
			keep[recordKey{typ: r.typ, name: strings.ToLower(v.DNSLabel)}] = struct{}{}
			result, err := t.syncRecord(ctx, v.DNSLabel, r.typ, r.ip)
			if err != nil {
				slog.Error("sync record failed",
					slog.String("record", v.DNSLabel),
					slog.String("type", r.typ),
					slog.Any("error", err),
				)
				failed++
				continue
			}
			switch result {
			case resultCreated:
				created++
			case resultUpdated:
				updated++
			default:
				unchanged++
			}
		}
	}

	removed, cleanupFailed := t.cleanupRecords(ctx, keep)
	failed += cleanupFailed

	slog.Info("sync completed",
		slog.Int("created", created),
		slog.Int("updated", updated),
		slog.Int("unchanged", unchanged),
		slog.Int("removed", removed),
		slog.Int("skipped", skipped),
		slog.Int("failed", failed),
		slog.String("duration", time.Since(start).Round(time.Millisecond).String()),
	)
	if failed > 0 {
		return fmt.Errorf("%d record operations failed", failed)
	}
	return nil
}

// skipReason returns why a peer gets no record, or "" if it should be synced.
func (t *Sync) skipReason(p netbird.Peer) string {
	if p.IP == "" && p.IPv6 == "" {
		return "no ip"
	}
	if _, ok := t.recordName(p.DNSLabel); !ok {
		return "not under zone"
	}
	return ""
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
	// JoinPath tolerates a trailing slash on the endpoint
	u, err := url.JoinPath(t.cfg.NetbirdEndpoint, "api", "peers")
	if err != nil {
		return nil, fmt.Errorf("invalid netbird endpoint: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("http get: %s: %s", resp.Status, strings.TrimSpace(string(body)))
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
	slog.Debug("cloudflare zone resolved",
		slog.String("zone", t.cfg.CloudflareDomain),
		slog.String("zone_id", t.cloudflareZoneID),
	)
	return nil
}

// recordBody is a record param accepted by both create and update.
type recordBody interface {
	dns.RecordNewParamsBodyUnion
	dns.RecordUpdateParamsBodyUnion
}

func recordParam(typ string, domain string, ipStr string, proxied bool) recordBody {
	if typ == typeAAAA {
		return dns.AAAARecordParam{
			Type:    cloudflare.F(dns.AAAARecordTypeAAAA),
			Name:    cloudflare.String(domain),
			Content: cloudflare.String(ipStr),
			Proxied: cloudflare.Bool(proxied),
			Comment: cloudflare.String(managedComment),
		}
	}
	return dns.ARecordParam{
		Type:    cloudflare.F(dns.ARecordTypeA),
		Name:    cloudflare.String(domain),
		Content: cloudflare.String(ipStr),
		Proxied: cloudflare.Bool(proxied),
		Comment: cloudflare.String(managedComment),
	}
}

// sameIP compares addresses by value, since Cloudflare may format IPv6 differently.
func sameIP(a, b string) bool {
	x, errX := netip.ParseAddr(a)
	y, errY := netip.ParseAddr(b)
	if errX != nil || errY != nil {
		return a == b
	}
	return x == y
}

func (t *Sync) createRecord(ctx context.Context, domain string, typ string, ipStr string) error {
	_, err := t.cloudflare.DNS.Records.New(ctx, dns.RecordNewParams{
		ZoneID: cloudflare.String(t.cloudflareZoneID),
		Body:   recordParam(typ, domain, ipStr, false),
	})
	if err != nil {
		return fmt.Errorf("create record: %w", err)
	}

	return nil
}

func (t *Sync) syncRecord(ctx context.Context, domain string, typ string, ipStr string) (syncResult, error) {
	list, err := t.cloudflare.DNS.Records.List(ctx, dns.RecordListParams{
		ZoneID: cloudflare.String(t.cloudflareZoneID),
		Name: cloudflare.F(dns.RecordListParamsName{
			Exact: cloudflare.F(domain),
		}),
		Type: cloudflare.F(dns.RecordListParamsType(typ)),
	})
	if err != nil {
		return 0, fmt.Errorf("list record(%s): %w", domain, err)
	}

	// create if empty
	if len(list.Result) == 0 {
		if err := t.createRecord(ctx, domain, typ, ipStr); err != nil {
			return 0, err
		}
		slog.Info("dns record created", slog.String("record", domain), slog.String("type", typ), slog.String("ip", ipStr))
		return resultCreated, nil
	}

	keepIdx := -1
	for i, record := range list.Result {
		if sameIP(record.Content, ipStr) {
			keepIdx = i
			break
		}
	}

	result := resultUpdated
	var keepID string
	switch {
	case keepIdx < 0:
		target := list.Result[0]
		if err := t.updateRecord(ctx, target.ID, domain, typ, ipStr, target.Proxied); err != nil {
			return 0, err
		}
		keepID = target.ID
		slog.Info("dns record updated",
			slog.String("record", domain),
			slog.String("type", typ),
			slog.String("ip", ipStr),
			slog.String("old_ip", target.Content),
		)
	case list.Result[keepIdx].Comment != managedComment:
		// adopt records created before ownership marking, so they can be cleaned up later
		target := list.Result[keepIdx]
		if err := t.updateRecord(ctx, target.ID, domain, typ, ipStr, target.Proxied); err != nil {
			return 0, err
		}
		keepID = target.ID
		slog.Info("dns record adopted", slog.String("record", domain), slog.String("type", typ), slog.String("ip", ipStr))
	default:
		keepID = list.Result[keepIdx].ID
		result = resultUnchanged
		slog.Debug("dns record unchanged", slog.String("record", domain), slog.String("type", typ), slog.String("ip", ipStr))
	}

	for _, record := range list.Result {
		if record.ID == keepID {
			continue
		}
		if err := t.deleteRecord(ctx, record.ID); err != nil {
			return 0, err
		}
		result = resultUpdated
		slog.Info("duplicate dns record removed",
			slog.String("record", domain),
			slog.String("type", typ),
			slog.String("ip", record.Content),
		)
	}

	return result, nil
}

func (t *Sync) updateRecord(ctx context.Context, recordID string, domain string, typ string, ipStr string, proxied bool) error {
	_, err := t.cloudflare.DNS.Records.Update(ctx, recordID, dns.RecordUpdateParams{
		ZoneID: cloudflare.String(t.cloudflareZoneID),
		Body:   recordParam(typ, domain, ipStr, proxied),
	})
	if err != nil {
		return fmt.Errorf("update record(%s): %w", domain, err)
	}
	return nil
}

// cleanupRecords removes managed A and AAAA records that are not in keep,
// logging each failure and returning how many records were removed and failed.
func (t *Sync) cleanupRecords(ctx context.Context, keep map[recordKey]struct{}) (removed, failed int) {
	iter := t.cloudflare.DNS.Records.ListAutoPaging(ctx, dns.RecordListParams{
		ZoneID: cloudflare.String(t.cloudflareZoneID),
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
		typ := string(record.Type)
		if typ != typeA && typ != typeAAAA {
			continue
		}
		if _, ok := keep[recordKey{typ: typ, name: strings.ToLower(record.Name)}]; ok {
			continue
		}
		stale = append(stale, record)
	}
	if err := iter.Err(); err != nil {
		slog.Error("list managed records failed", slog.Any("error", err))
		return 0, 1
	}

	for _, record := range stale {
		if err := t.deleteRecord(ctx, record.ID); err != nil {
			slog.Error("remove stale dns record failed",
				slog.String("record", record.Name),
				slog.String("type", string(record.Type)),
				slog.Any("error", err),
			)
			failed++
			continue
		}
		slog.Info("stale dns record removed",
			slog.String("record", record.Name),
			slog.String("type", string(record.Type)),
			slog.String("ip", record.Content),
		)
		removed++
	}
	return removed, failed
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
