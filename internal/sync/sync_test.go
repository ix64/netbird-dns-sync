package sync

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/option"

	stdsync "sync"

	"github.com/ix64/netbird-dns-sync/internal/netbird"
)

type fakeRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Comment string `json:"comment"`
}

type fakeServer struct {
	mu      stdsync.Mutex
	records []fakeRecord
	failFor string // record name whose creation fails
	calls   []string
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)

	reply := func(status int, result any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": status < 300, "errors": []any{}, "messages": []any{}, "result": result,
		})
	}

	switch {
	case r.URL.Path == "/api/peers":
		_ = json.NewEncoder(w).Encode([]netbird.Peer{
			{DNSLabel: "new.example.com", IP: "100.64.0.1"},
			{DNSLabel: "same.example.com", IP: "100.64.0.2"},
			{DNSLabel: "changed.example.com", IP: "100.64.0.3"},
			{DNSLabel: "other.netbird.cloud", IP: "100.64.0.4"},
			{DNSLabel: "broken.example.com", IP: "100.64.0.5"},
			{DNSLabel: "dup.example.com", IP: "100.64.0.6"},
			{DNSLabel: "adopt.example.com", IP: "100.64.0.7"},
			{DNSLabel: "Mixed.example.com", IP: "100.64.0.8"},
		})

	case r.URL.Path == "/zones":
		reply(http.StatusOK, []map[string]any{{"id": "zone1", "name": "example.com"}})

	case r.URL.Path == "/zones/zone1/dns_records" && r.Method == http.MethodGet:
		q := r.URL.Query()
		if page := q.Get("page"); page != "" && page != "1" {
			reply(http.StatusOK, []fakeRecord{})
			return
		}
		var out []fakeRecord
		for _, rec := range f.records {
			if q.Has("name.exact") && !strings.EqualFold(rec.Name, q.Get("name.exact")) {
				continue
			}
			if q.Has("comment.exact") && !strings.EqualFold(rec.Comment, q.Get("comment.exact")) {
				continue
			}
			out = append(out, rec)
		}
		reply(http.StatusOK, out)

	case r.URL.Path == "/zones/zone1/dns_records" && r.Method == http.MethodPost:
		var rec fakeRecord
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &rec)
		rec.Name += ".example.com"
		if rec.Name == f.failFor {
			reply(http.StatusBadRequest, nil)
			return
		}
		rec.ID = "id-" + rec.Name
		f.records = append(f.records, rec)
		reply(http.StatusOK, rec)

	case strings.HasPrefix(r.URL.Path, "/zones/zone1/dns_records/") && r.Method == http.MethodPut:
		id := strings.TrimPrefix(r.URL.Path, "/zones/zone1/dns_records/")
		var rec fakeRecord
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &rec)
		for i := range f.records {
			if f.records[i].ID == id {
				f.records[i].Content = rec.Content
				f.records[i].Comment = rec.Comment
			}
		}
		reply(http.StatusOK, rec)

	case strings.HasPrefix(r.URL.Path, "/zones/zone1/dns_records/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(r.URL.Path, "/zones/zone1/dns_records/")
		f.records = slices.DeleteFunc(f.records, func(rec fakeRecord) bool { return rec.ID == id })
		reply(http.StatusOK, map[string]any{"id": id})

	default:
		http.NotFound(w, r)
	}
}

func TestRun(t *testing.T) {
	fake := &fakeServer{
		failFor: "broken.example.com",
		records: []fakeRecord{
			{ID: "r-same", Name: "same.example.com", Type: "A", Content: "100.64.0.2", Comment: managedComment},
			{ID: "r-changed", Name: "changed.example.com", Type: "A", Content: "1.1.1.1"},
			{ID: "r-dup1", Name: "dup.example.com", Type: "A", Content: "2.2.2.2"},
			{ID: "r-dup2", Name: "dup.example.com", Type: "A", Content: "100.64.0.6"},
			{ID: "r-adopt", Name: "adopt.example.com", Type: "A", Content: "100.64.0.7", Comment: "set by hand"},
			{ID: "r-mixed", Name: "mixed.example.com", Type: "A", Content: "100.64.0.8", Comment: managedComment},
			{ID: "r-stale", Name: "gone.example.com", Type: "A", Content: "100.64.0.9", Comment: managedComment},
			{ID: "r-manual", Name: "manual.example.com", Type: "A", Content: "1.2.3.4"},
			{ID: "r-similar", Name: "similar.example.com", Type: "A", Content: "1.2.3.5", Comment: strings.ToUpper(managedComment)},
		},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	s := New(&Config{
		NetbirdEndpoint:    srv.URL,
		NetbirdAccessToken: "token",
		CloudflareAPIToken: "token",
		CloudflareDomain:   "example.com",
	})
	s.cloudflare = cloudflare.NewClient(
		option.WithAPIToken("token"),
		option.WithBaseURL(srv.URL),
		option.WithMaxRetries(0),
	)

	err := s.Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "broken.example.com") {
		t.Fatalf("expected error for broken.example.com, got %v", err)
	}

	got := map[string][]string{}
	for _, rec := range fake.records {
		got[rec.Name] = append(got[rec.Name], rec.Content)
	}
	want := map[string][]string{
		"new.example.com":     {"100.64.0.1"},
		"same.example.com":    {"100.64.0.2"},
		"changed.example.com": {"100.64.0.3"},
		"dup.example.com":     {"100.64.0.6"},
		"adopt.example.com":   {"100.64.0.7"},
		"mixed.example.com":   {"100.64.0.8"},
		"manual.example.com":  {"1.2.3.4"},
		"similar.example.com": {"1.2.3.5"},
	}
	if len(got) != len(want) {
		t.Fatalf("records = %v, want %v", got, want)
	}
	for name, ips := range want {
		if !slices.Equal(got[name], ips) {
			t.Errorf("records[%s] = %v, want %v", name, got[name], ips)
		}
	}

	for _, rec := range fake.records {
		unmanaged := rec.Name == "manual.example.com" || rec.Name == "similar.example.com"
		if !unmanaged && rec.Comment != managedComment {
			t.Errorf("records[%s].comment = %q, want %q", rec.Name, rec.Comment, managedComment)
		}
	}

	for _, call := range fake.calls {
		if strings.Contains(call, "r-same") || strings.Contains(call, "r-mixed") {
			t.Errorf("unexpected call on up-to-date record: %s", call)
		}
	}
}

func TestRecordName(t *testing.T) {
	s := &Sync{cfg: &Config{CloudflareDomain: "example.com"}}
	tests := []struct {
		domain string
		name   string
		ok     bool
	}{
		{"peer.example.com", "peer", true},
		{"a.b.example.com", "a.b", true},
		{"example.com", "", false},
		{".example.com", "", false},
		{"peer.notexample.com", "", false},
		{"peer.example.com.evil", "", false},
	}
	for _, tt := range tests {
		name, ok := s.recordName(tt.domain)
		if name != tt.name || ok != tt.ok {
			t.Errorf("recordName(%q) = %q, %v; want %q, %v", tt.domain, name, ok, tt.name, tt.ok)
		}
	}
}
