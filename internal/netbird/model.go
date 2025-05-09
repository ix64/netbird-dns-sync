package netbird

import "time"

type PeerGroup struct {
	Id             string `json:"id"`
	Name           string `json:"name"`
	PeersCount     int    `json:"peers_count"`
	ResourcesCount int    `json:"resources_count"`
}

type Peer struct {
	ID     string `json:"id"`
	UserID string `json:"user_id"`

	IP       string `json:"ip"`
	DNSLabel string `json:"dns_label"`

	AccessiblePeersCount        int         `json:"accessible_peers_count"`
	ApprovalRequired            bool        `json:"approval_required"`
	CityName                    string      `json:"city_name"`
	Connected                   bool        `json:"connected"`
	ConnectionIP                string      `json:"connection_ip"`
	CountryCode                 string      `json:"country_code"`
	GeonameID                   int         `json:"geoname_id"`
	Groups                      []PeerGroup `json:"groups"`
	Hostname                    string      `json:"hostname"`
	InactivityExpirationEnabled bool        `json:"inactivity_expiration_enabled"`
	KernelVersion               string      `json:"kernel_version"`
	LastLogin                   time.Time   `json:"last_login"`
	LastSeen                    time.Time   `json:"last_seen"`
	LoginExpirationEnabled      bool        `json:"login_expiration_enabled"`
	LoginExpired                bool        `json:"login_expired"`
	Name                        string      `json:"name"`
	OS                          string      `json:"os"`
	SerialNumber                string      `json:"serial_number"`
	SSHEnabled                  bool        `json:"ssh_enabled"`
	UIVersion                   string      `json:"ui_version"`
	Version                     string      `json:"version"`
}
