package config

import "encoding/json"

const (
	// DefaultMaxConnections is used when a rule leaves max_connections at zero.
	DefaultMaxConnections = 8192
	// MaxConnections is the largest explicit per-rule connection limit.
	MaxConnections = 65536
)

type Rule struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Enabled        bool     `json:"enabled"`
	Network        string   `json:"network"`
	Listen         string   `json:"listen"`
	Target         string   `json:"target"`
	AllowedCIDRs   []string `json:"allowed_cidrs,omitempty"`
	MaxConnections int      `json:"max_connections"`
	UDPTimeout     int      `json:"udp_timeout"`
}

type Config struct {
	Version int    `json:"version"`
	Rules   []Rule `json:"rules"`
	Engine  Engine `json:"engine"`
}

type Engine struct {
	Enabled bool            `json:"enabled"`
	Config  json.RawMessage `json:"config"`
}
