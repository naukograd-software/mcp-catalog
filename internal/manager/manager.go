package manager

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/naukograd-software/mcp-catalog/internal/config"
)

type ServerStatus string

const (
	StatusUnchecked ServerStatus = "unchecked"
	StatusChecking  ServerStatus = "checking"
	StatusHealthy   ServerStatus = "healthy"
	StatusError     ServerStatus = "error"
)

type LogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

type ServerInfo struct {
	Name            string           `json:"name"`
	Config          config.MCPServer `json:"config"`
	Status          ServerStatus     `json:"status"`
	Error           string           `json:"error,omitempty"`
	Logs            []LogEntry       `json:"logs"`
	Tools           []MCPTool        `json:"tools"`
	LastCheck       *time.Time       `json:"lastCheck,omitempty"`
	ServerName      string           `json:"serverName,omitempty"`
	ServerVersion   string           `json:"serverVersion,omitempty"`
	ProtocolVersion string           `json:"protocolVersion,omitempty"`
	CheckDuration   int64            `json:"checkDuration,omitempty"`
}

type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

type mcpToolsResult struct {
	Tools []MCPTool `json:"tools"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpInitResult struct {
	ProtocolVersion string            `json:"protocolVersion"`
	ServerInfo      mcpServerInfoResp `json:"serverInfo"`
}

type mcpServerInfoResp struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

const maxLogEntries = 500
const checkTimeout = 30 * time.Second

type Manager struct {
	store          *config.Store
	servers        map[string]*ServerInfo
	mu             sync.RWMutex
	listeners      []func(name string, info *ServerInfo)
	listMu         sync.RWMutex
	healthInterval int
	healthMu       sync.RWMutex
	stopHealth     chan struct{}
}

func New(store *config.Store) *Manager {
	return &Manager{
		store:          store,
		servers:        make(map[string]*ServerInfo),
		healthInterval: store.GetHealthCheckInterval(),
		stopHealth:     make(chan struct{}),
	}
}

func (m *Manager) GetHealthInterval() int {
	m.healthMu.RLock()
	defer m.healthMu.RUnlock()
	return m.healthInterval
}

func (m *Manager) SetHealthInterval(seconds int) {
	m.healthMu.Lock()
	m.healthInterval = seconds
	m.healthMu.Unlock()
}

func (m *Manager) OnChange(fn func(name string, info *ServerInfo)) {
	m.listMu.Lock()
	defer m.listMu.Unlock()
	m.listeners = append(m.listeners, fn)
}

func (m *Manager) notify(name string, info *ServerInfo) {
	m.listMu.RLock()
	defer m.listMu.RUnlock()
	for _, fn := range m.listeners {
		go fn(name, info)
	}
}

func (m *Manager) getOrCreateInfo(name string) *ServerInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	if info, ok := m.servers[name]; ok {
		return info
	}

	srv, ok := m.store.GetServer(name)
	if !ok {
		return nil
	}

	info := &ServerInfo{
		Name:   name,
		Config: *srv,
		Status: StatusUnchecked,
		Logs:   make([]LogEntry, 0),
		Tools:  make([]MCPTool, 0),
	}
	m.servers[name] = info
	return info
}

func (m *Manager) addLog(info *ServerInfo, level, msg string) {
	entry := LogEntry{
		Time:    time.Now(),
		Level:   level,
		Message: msg,
	}
	info.Logs = append(info.Logs, entry)
	if len(info.Logs) > maxLogEntries {
		info.Logs = info.Logs[len(info.Logs)-maxLogEntries:]
	}
}

// Check starts the server temporarily, verifies MCP initialize works, discovers tools, then stops it.
func (m *Manager) Check(name string) error {
	srv, ok := m.store.GetServer(name)
	if !ok {
		return fmt.Errorf("server %q not found", name)
	}

	info := m.getOrCreateInfo(name)
	if info == nil {
		return fmt.Errorf("server %q not found", name)
	}

	// Mark as checking
	m.mu.Lock()
	info.Status = StatusChecking
	info.Error = ""
	info.Config = *srv
	m.mu.Unlock()
	target := strings.TrimSpace(strings.Join(append([]string{srv.Command}, srv.Args...), " "))
	if isStreamableHTTPServer(srv) {
		target = fmt.Sprintf("streamableHttp %s", srv.URL)
	}
	if target == "" {
		target = "(invalid config: no command/url)"
	}
	m.addLog(info, "info", fmt.Sprintf("Checking: %s", target))
	m.notify(name, info)

	// Run the actual check
	err := m.doCheck(name, srv, info)

	now := time.Now()
	m.mu.Lock()
	info.LastCheck = &now
	if err != nil {
		info.Status = StatusError
		info.Error = err.Error()
	} else {
		info.Status = StatusHealthy
		info.Error = ""
	}
	m.mu.Unlock()
	m.notify(name, info)

	return err
}

func (m *Manager) doCheck(name string, srv *config.MCPServer, info *ServerInfo) error {
	_ = name
	if isStreamableHTTPServer(srv) {
		return m.doCheckStreamableHTTP(srv, info)
	}
	if srv.Command == "" {
		err := fmt.Errorf("missing command for stdio server")
		m.addLog(info, "error", err.Error())
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, srv.Command, srv.Args...)

	if len(srv.Env) > 0 {
		env := cmd.Environ()
		for k, v := range srv.Env {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
		cmd.Env = env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		m.addLog(info, "error", fmt.Sprintf("stdin pipe: %v", err))
		return fmt.Errorf("stdin pipe: %w", err)
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		m.addLog(info, "error", fmt.Sprintf("stdout pipe: %v", err))
		return fmt.Errorf("stdout pipe: %w", err)
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		m.addLog(info, "error", fmt.Sprintf("stderr pipe: %v", err))
		return fmt.Errorf("stderr pipe: %w", err)
	}

	startTime := time.Now()

	if err := cmd.Start(); err != nil {
		info.CheckDuration = time.Since(startTime).Milliseconds()
		m.addLog(info, "error", fmt.Sprintf("Failed to start: %v", err))
		return fmt.Errorf("start: %w", err)
	}
	m.addLog(info, "info", fmt.Sprintf("Started with PID %d", cmd.Process.Pid))

	// Collect stderr in background
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(stderrPipe)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for scanner.Scan() {
			m.addLog(info, "stderr", scanner.Text())
		}
	}()

	stdout := bufio.NewReader(stdoutPipe)

	// Send MCP initialize
	initReq := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"mcp-manager","version":"1.0.0"}}}` + "\n"
	if _, err := stdin.Write([]byte(initReq)); err != nil {
		cancel()
		m.addLog(info, "error", fmt.Sprintf("Failed to send initialize: %v", err))
		return fmt.Errorf("send initialize: %w", err)
	}

	// Read initialize response
	line, err := stdout.ReadString('\n')
	if err != nil {
		cancel()
		m.addLog(info, "error", fmt.Sprintf("Failed to read initialize response: %v", err))
		return fmt.Errorf("read initialize response: %w", err)
	}

	var initResp mcpResponse
	if err := json.Unmarshal([]byte(line), &initResp); err != nil {
		cancel()
		m.addLog(info, "error", fmt.Sprintf("Invalid initialize response: %v", err))
		return fmt.Errorf("parse initialize response: %w", err)
	}

	if initResp.Error != nil {
		cancel()
		info.CheckDuration = time.Since(startTime).Milliseconds()
		m.addLog(info, "error", fmt.Sprintf("Initialize error: %s", initResp.Error.Message))
		return fmt.Errorf("initialize: %s", initResp.Error.Message)
	}

	// Extract server info from initialize result
	var initResult mcpInitResult
	if err := json.Unmarshal(initResp.Result, &initResult); err == nil {
		info.ServerName = initResult.ServerInfo.Name
		info.ServerVersion = initResult.ServerInfo.Version
		info.ProtocolVersion = initResult.ProtocolVersion
	}

	m.addLog(info, "info", fmt.Sprintf("MCP initialized: %s %s (protocol %s)",
		info.ServerName, info.ServerVersion, info.ProtocolVersion))

	// Send initialized notification
	notif := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
	stdin.Write([]byte(notif))

	// List tools
	toolsReq := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}` + "\n"
	if _, err := stdin.Write([]byte(toolsReq)); err != nil {
		cancel()
		m.addLog(info, "warn", fmt.Sprintf("Failed to send tools/list: %v", err))
		// Not a fatal error — initialize succeeded
		return nil
	}

	line, err = stdout.ReadString('\n')
	if err != nil {
		cancel()
		m.addLog(info, "warn", fmt.Sprintf("Failed to read tools/list response: %v", err))
		return nil
	}

	var toolsResp mcpResponse
	if err := json.Unmarshal([]byte(line), &toolsResp); err != nil {
		m.addLog(info, "warn", fmt.Sprintf("Invalid tools/list response: %v", err))
	} else if toolsResp.Error != nil {
		m.addLog(info, "warn", fmt.Sprintf("tools/list error: %s", toolsResp.Error.Message))
	} else {
		var result mcpToolsResult
		if err := json.Unmarshal(toolsResp.Result, &result); err != nil {
			m.addLog(info, "warn", fmt.Sprintf("Failed to parse tools: %v", err))
		} else {
			m.mu.Lock()
			info.Tools = result.Tools
			m.mu.Unlock()
			m.addLog(info, "info", fmt.Sprintf("Discovered %d tools", len(result.Tools)))
		}
	}

	// Kill the process
	cancel()
	cmd.Wait()
	<-stderrDone

	info.CheckDuration = time.Since(startTime).Milliseconds()
	m.addLog(info, "info", fmt.Sprintf("Check completed in %dms, process stopped", info.CheckDuration))

	return nil
}

func isStreamableHTTPServer(srv *config.MCPServer) bool {
	if srv == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(srv.Type), "streamableHttp") {
		return true
	}
	return strings.TrimSpace(srv.URL) != "" && strings.TrimSpace(srv.Command) == ""
}

func (m *Manager) doCheckStreamableHTTP(srv *config.MCPServer, info *ServerInfo) error {
	if srv.URL == "" {
		err := fmt.Errorf("missing url for streamableHttp server")
		m.addLog(info, "error", err.Error())
		return err
	}

	startTime := time.Now()
	m.addLog(info, "info", fmt.Sprintf("Connecting via streamable HTTP: %s", srv.URL))
	client := &http.Client{Timeout: checkTimeout}

	send := func(payload map[string]any, expectResponse bool) (*mcpResponse, error) {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}

		req, err := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("send request: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return nil, fmt.Errorf("http status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}

		if !expectResponse {
			io.Copy(io.Discard, resp.Body)
			return nil, nil
		}

		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}

		parsed, err := decodeHTTPMCPResponse(raw)
		if err != nil {
			return nil, err
		}
		return parsed, nil
	}

	initReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "mcp-manager",
				"version": "1.0.0",
			},
		},
	}

	initResp, err := send(initReq, true)
	if err != nil {
		info.CheckDuration = time.Since(startTime).Milliseconds()
		m.addLog(info, "error", fmt.Sprintf("Initialize request failed: %v", err))
		return fmt.Errorf("initialize request: %w", err)
	}

	if initResp.Error != nil {
		info.CheckDuration = time.Since(startTime).Milliseconds()
		m.addLog(info, "error", fmt.Sprintf("Initialize error: %s", initResp.Error.Message))
		return fmt.Errorf("initialize: %s", initResp.Error.Message)
	}

	var initResult mcpInitResult
	if err := json.Unmarshal(initResp.Result, &initResult); err == nil {
		info.ServerName = initResult.ServerInfo.Name
		info.ServerVersion = initResult.ServerInfo.Version
		info.ProtocolVersion = initResult.ProtocolVersion
	}
	m.addLog(info, "info", fmt.Sprintf("MCP initialized: %s %s (protocol %s)",
		info.ServerName, info.ServerVersion, info.ProtocolVersion))

	notif := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}
	if _, err := send(notif, false); err != nil {
		m.addLog(info, "warn", fmt.Sprintf("Failed to send initialized notification: %v", err))
	}

	toolsReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
		"params":  map[string]any{},
	}
	toolsResp, err := send(toolsReq, true)
	if err != nil {
		info.CheckDuration = time.Since(startTime).Milliseconds()
		m.addLog(info, "warn", fmt.Sprintf("tools/list request failed: %v", err))
		return nil
	}

	if toolsResp.Error != nil {
		m.addLog(info, "warn", fmt.Sprintf("tools/list error: %s", toolsResp.Error.Message))
	} else {
		var result mcpToolsResult
		if err := json.Unmarshal(toolsResp.Result, &result); err != nil {
			m.addLog(info, "warn", fmt.Sprintf("Failed to parse tools: %v", err))
		} else {
			m.mu.Lock()
			info.Tools = result.Tools
			m.mu.Unlock()
			m.addLog(info, "info", fmt.Sprintf("Discovered %d tools", len(result.Tools)))
		}
	}

	info.CheckDuration = time.Since(startTime).Milliseconds()
	m.addLog(info, "info", fmt.Sprintf("Check completed in %dms", info.CheckDuration))
	return nil
}

func decodeHTTPMCPResponse(raw []byte) (*mcpResponse, error) {
	data := strings.TrimSpace(string(raw))
	if data == "" {
		return nil, fmt.Errorf("empty response body")
	}

	var resp mcpResponse
	if err := json.Unmarshal([]byte(data), &resp); err == nil && (resp.JSONRPC != "" || resp.Result != nil || resp.Error != nil) {
		return &resp, nil
	}

	var batch []mcpResponse
	if err := json.Unmarshal([]byte(data), &batch); err == nil && len(batch) > 0 {
		return &batch[0], nil
	}

	// Fallback for SSE replies where payload comes as "data: {json}" lines.
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if err := json.Unmarshal([]byte(payload), &resp); err == nil {
			return &resp, nil
		}
	}

	return nil, fmt.Errorf("unable to decode MCP response: %s", data)
}

// CheckAll checks all enabled servers.
func (m *Manager) CheckAll() {
	cfg := m.store.Get()
	for name, srv := range cfg.MCPServers {
		if srv.Enabled {
			m.Check(name)
		}
	}
}

// StartHealthLoop runs periodic health checks in background.
func (m *Manager) StartHealthLoop() {
	for {
		m.healthMu.RLock()
		interval := m.healthInterval
		m.healthMu.RUnlock()

		if interval <= 0 {
			// Disabled, poll every 5s to see if it gets enabled
			select {
			case <-m.stopHealth:
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}

		select {
		case <-m.stopHealth:
			return
		case <-time.After(time.Duration(interval) * time.Second):
			m.CheckAll()
		}
	}
}

// StopHealthLoop stops the background health loop.
func (m *Manager) StopHealthLoop() {
	close(m.stopHealth)
}

// RemoveServer removes cached info for a deleted server.
func (m *Manager) RemoveServer(name string) {
	m.mu.Lock()
	delete(m.servers, name)
	m.mu.Unlock()
}

func (m *Manager) GetInfo(name string) (*ServerInfo, bool) {
	m.mu.RLock()
	info, ok := m.servers[name]
	m.mu.RUnlock()
	if !ok {
		srv, ok := m.store.GetServer(name)
		if !ok {
			return nil, false
		}
		return &ServerInfo{
			Name:   name,
			Config: *srv,
			Status: StatusUnchecked,
			Logs:   []LogEntry{},
			Tools:  []MCPTool{},
		}, true
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	// Return a copy
	cp := *info
	cp.Logs = make([]LogEntry, len(info.Logs))
	copy(cp.Logs, info.Logs)
	cp.Tools = make([]MCPTool, len(info.Tools))
	copy(cp.Tools, info.Tools)
	return &cp, true
}

func (m *Manager) GetAllInfo() map[string]*ServerInfo {
	cfg := m.store.Get()
	result := make(map[string]*ServerInfo)
	for name := range cfg.MCPServers {
		info, ok := m.GetInfo(name)
		if ok {
			result[name] = info
		}
	}
	return result
}
