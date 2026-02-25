package config

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
)

// MCPServer represents a single MCP server configuration
// Compatible with Claude/Codex mcpServers format
type MCPServer struct {
	Type    string            `json:"type,omitempty"`
	URL     string            `json:"url,omitempty"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Enabled bool              `json:"enabled"`
}

// Config holds the full configuration
type Config struct {
	MCPServers          map[string]*MCPServer `json:"mcpServers"`
	HealthCheckInterval int                   `json:"healthCheckInterval,omitempty"`
}

// Store manages config persistence
type Store struct {
	mu     sync.RWMutex
	path   string
	config *Config
}

func normalizeServer(srv *MCPServer) {
	if srv == nil {
		return
	}
	srv.Type = strings.TrimSpace(srv.Type)
	srv.URL = strings.TrimSpace(srv.URL)
	srv.Command = strings.TrimSpace(srv.Command)
	if srv.URL != "" && srv.Type == "" {
		srv.Type = "streamableHttp"
	}
}

func normalizeConfig(cfg *Config) {
	if cfg == nil {
		return
	}
	if cfg.MCPServers == nil {
		cfg.MCPServers = make(map[string]*MCPServer)
	}
	for _, srv := range cfg.MCPServers {
		normalizeServer(srv)
	}
}

func NewStore(path string) *Store {
	return &Store{
		path: path,
		config: &Config{
			MCPServers: make(map[string]*MCPServer),
		},
	}
}

func (s *Store) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s.saveLocked()
		}
		return err
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return err
	}
	normalizeConfig(&cfg)
	s.config = &cfg
	return nil
}

func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(s.config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0644)
}

func (s *Store) Get() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Return a copy
	cp := &Config{MCPServers: make(map[string]*MCPServer)}
	for k, v := range s.config.MCPServers {
		srv := *v
		cp.MCPServers[k] = &srv
	}
	return cp
}

func (s *Store) Set(cfg *Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	normalizeConfig(cfg)
	s.config = cfg
	return s.saveLocked()
}

func (s *Store) AddServer(name string, srv *MCPServer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	normalizeServer(srv)
	s.config.MCPServers[name] = srv
	return s.saveLocked()
}

func (s *Store) RemoveServer(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.config.MCPServers, name)
	return s.saveLocked()
}

func (s *Store) GetServer(name string) (*MCPServer, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	srv, ok := s.config.MCPServers[name]
	if !ok {
		return nil, false
	}
	cp := *srv
	return &cp, true
}

func (s *Store) GetHealthCheckInterval() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config.HealthCheckInterval
}

func (s *Store) SetHealthCheckInterval(seconds int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config.HealthCheckInterval = seconds
	return s.saveLocked()
}

func (s *Store) Export() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return json.MarshalIndent(s.config, "", "  ")
}
