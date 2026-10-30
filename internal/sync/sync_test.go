package sync

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
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
			{DNSLabel: "new.example.com", IP: "100.64.0.1", IPv6: "fd00::1"},
			{DNSLabel: "same.example.com", IP: "100.64.0.2"},
			{DNSLabel: "changed.example.com", IP: "100.64.0.3"},
			{DNSLabel: "other.netbird.cloud", IP: "100.64.0.4"},
			{DNSLabel: "broken.example.com", IP: "100.64.0.5"},
			{DNSLabel: "dup.example.com", IP: "100.64.0.6"},
			{DNSLabel: "adopt.example.com", IP: "100.64.0.7"},
			{DNSLabel: "Mixed.example.com", IP: "100.64.0.8"},
			{DNSLabel: "v6only.example.com", IPv6: "fd00::2"},
			{DNSLabel: "norm.example.com", IP: "100.64.0.10", IPv6: "fd00::3"},
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
			if q.Has("type") && rec.Type != q.Get("type") {
				continue
			}
			out = append(out, rec)
		}
		reply(http.StatusOK, out)

	case r.URL.Path == "/zones/zone1/dns_records" && r.Method == http.MethodPost:
		var rec fakeRecord
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &rec)
		// stored as sent: the sync must always send full names
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
			// peer has no IPv6 any more
			{ID: "r-same6", Name: "same.example.com", Type: "AAAA", Content: "fd00::99", Comment: managedComment},
			// same addresses, IPv6 written in expanded form
			{ID: "r-norm", Name: "norm.example.com", Type: "A", Content: "100.64.0.10", Comment: managedComment},
			{ID: "r-norm6", Name: "norm.example.com", Type: "AAAA", Content: "fd00:0:0:0:0:0:0:3", Comment: managedComment},
			// only A and AAAA records are ever cleaned up
			{ID: "r-txt", Name: "txt.example.com", Type: "TXT", Content: "hello", Comment: managedComment},
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

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	err := s.Run(t.Context())
	if err == nil || err.Error() != "1 record operations failed" {
		t.Fatalf("expected 1 failed operation, got %v", err)
	}

	out := logs.String()
	for _, want := range []string{
		`msg="sync record failed" record=broken.example.com`,
		`msg="sync completed" created=3 updated=3 unchanged=4 removed=2 skipped=1 failed=1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("logs missing %q:\n%s", want, out)
		}
	}
	// per-peer noise stays at debug
	for _, noise := range []string{"dns record unchanged", "peer skipped"} {
		if strings.Contains(out, noise) {
			t.Errorf("logs contain debug message %q at info level:\n%s", noise, out)
		}
	}

	got := map[string][]string{}
	for _, rec := range fake.records {
		key := rec.Type + " " + rec.Name
		got[key] = append(got[key], rec.Content)
	}
	want := map[string][]string{
		"A new.example.com":       {"100.64.0.1"},
		"AAAA new.example.com":    {"fd00::1"},
		"A same.example.com":      {"100.64.0.2"},
		"A changed.example.com":   {"100.64.0.3"},
		"A dup.example.com":       {"100.64.0.6"},
		"A adopt.example.com":     {"100.64.0.7"},
		"A mixed.example.com":     {"100.64.0.8"},
		"AAAA v6only.example.com": {"fd00::2"},
		"A norm.example.com":      {"100.64.0.10"},
		"AAAA norm.example.com":   {"fd00:0:0:0:0:0:0:3"},
		"A manual.example.com":    {"1.2.3.4"},
		"A similar.example.com":   {"1.2.3.5"},
		"TXT txt.example.com":     {"hello"},
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
		if strings.HasSuffix(call, "/r-same") || strings.Contains(call, "r-mixed") || strings.Contains(call, "r-norm") {
			t.Errorf("unexpected call on up-to-date record: %s", call)
		}
	}
}

func TestGetPeersErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"token invalid"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	s := New(&Config{NetbirdEndpoint: srv.URL, NetbirdAccessToken: "token"})
	_, err := s.getPeers(t.Context())
	want := `http get: 401 Unauthorized: {"message":"token invalid"}`
	if err == nil || err.Error() != want {
		t.Fatalf("getPeers error = %v, want %q", err, want)
	}
}

func TestGetPeersTrailingSlash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/peers" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]netbird.Peer{{DNSLabel: "peer.example.com", IP: "100.64.0.1"}})
	}))
	defer srv.Close()

	s := New(&Config{NetbirdEndpoint: srv.URL + "/", NetbirdAccessToken: "token"})
	peers, err := s.getPeers(t.Context())
	if err != nil || len(peers) != 1 {
		t.Fatalf("getPeers = %v, %v; want 1 peer", peers, err)
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
