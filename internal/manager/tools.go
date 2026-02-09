package manager

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

type CLITool struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	ConfigPath  string `json:"configPath"`
	Installed   bool   `json:"installed"`
	HasConfig   bool   `json:"hasConfig"`
}

type DiffResult struct {
	ConfigPath string `json:"configPath"`
	Current    string `json:"current"`
	Proposed   string `json:"proposed"`
}

type toolDef struct {
	name        string
	displayName string
	binary      string
	configRel   string // relative to $HOME
	format      string // "json-mcpServers", "json-opencode", "toml-codex"
}

var knownTools = []toolDef{
	{"claude", "Claude Code", "claude", ".claude.json", "json-mcpServers"},
	{"cursor", "Cursor", "cursor", ".cursor/mcp.json", "json-mcpServers"},
	{"gemini", "Gemini CLI", "gemini", ".gemini/settings.json", "json-mcpServers"},
	{"codex", "Codex", "codex", ".codex/config.toml", "toml-codex"},
	{"opencode", "OpenCode", "opencode", ".config/opencode/opencode.json", "json-opencode"},
	{"kilo", "Kilo Code", "kilo", ".kilocode/mcp.json", "json-mcpServers"},
	{"antygravity", "Antygravity", "antygravity", ".gemini/antygravity/mcp_config.json", "json-mcpServers"},
}

func (m *Manager) DetectTools() []CLITool {
	home, _ := os.UserHomeDir()
	var result []CLITool

	for _, td := range knownTools {
		configPath := filepath.Join(home, td.configRel)
		_, binErr := exec.LookPath(td.binary)
		_, statErr := os.Stat(configPath)

		installed := binErr == nil
		hasConfig := statErr == nil

		if !installed && !hasConfig {
			continue
		}

		result = append(result, CLITool{
			Name:        td.name,
			DisplayName: td.displayName,
			ConfigPath:  configPath,
			Installed:   installed,
			HasConfig:   hasConfig,
		})
	}

	return result
}

func findToolDef(name string) *toolDef {
	for i := range knownTools {
		if knownTools[i].name == name {
			return &knownTools[i]
		}
	}
	return nil
}

func (m *Manager) PreviewApply(toolName string) (*DiffResult, error) {
	td := findToolDef(toolName)
	if td == nil {
		return nil, fmt.Errorf("unknown tool %q", toolName)
	}

	home, _ := os.UserHomeDir()
	configPath := filepath.Join(home, td.configRel)

	// Read current file
	current := ""
	data, err := os.ReadFile(configPath)
	if err == nil {
		current = string(data)
	}

	// Generate proposed
	proposed, err := m.generateProposed(td, current)
	if err != nil {
		return nil, err
	}

	return &DiffResult{
		ConfigPath: configPath,
		Current:    current,
		Proposed:   proposed,
	}, nil
}

func (m *Manager) ApplyToTool(toolName string) error {
	diff, err := m.PreviewApply(toolName)
	if err != nil {
		return err
	}

	dir := filepath.Dir(diff.ConfigPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	return os.WriteFile(diff.ConfigPath, []byte(diff.Proposed), 0644)
}

func (m *Manager) generateProposed(td *toolDef, current string) (string, error) {
	switch td.format {
	case "json-mcpServers":
		return m.proposedJSONMcpServers(current)
	case "json-opencode":
		return m.proposedJSONOpenCode(current)
	case "toml-codex":
		return m.proposedTOMLCodex(current)
	default:
		return "", fmt.Errorf("unsupported format %q", td.format)
	}
}

// enabledServersClean returns enabled servers with the "enabled" field stripped.
func (m *Manager) enabledServersClean() map[string]any {
	cfg := m.store.Get()
	result := make(map[string]any)
	for name, srv := range cfg.MCPServers {
		if !srv.Enabled {
			continue
		}
		entry := map[string]any{
			"command": srv.Command,
		}
		if len(srv.Args) > 0 {
			entry["args"] = srv.Args
		}
		if len(srv.Env) > 0 {
			entry["env"] = srv.Env
		}
		result[name] = entry
	}
	return result
}

// JSON format with "mcpServers" key (Claude, Cursor, Gemini)
func (m *Manager) proposedJSONMcpServers(current string) (string, error) {
	var doc map[string]any

	if current != "" {
		if err := json.Unmarshal([]byte(current), &doc); err != nil {
			// If current file is invalid JSON, start fresh
			doc = make(map[string]any)
		}
	} else {
		doc = make(map[string]any)
	}

	servers := m.enabledServersClean()

	// Merge: keep existing servers not managed by us, add/overwrite ours
	existing, _ := doc["mcpServers"].(map[string]any)
	if existing == nil {
		existing = make(map[string]any)
	}
	for name, srv := range servers {
		existing[name] = srv
	}
	doc["mcpServers"] = existing

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}

// OpenCode JSON format with "mcp" key
func (m *Manager) proposedJSONOpenCode(current string) (string, error) {
	var doc map[string]any

	if current != "" {
		if err := json.Unmarshal([]byte(current), &doc); err != nil {
			doc = make(map[string]any)
		}
	} else {
		doc = make(map[string]any)
	}

	cfg := m.store.Get()
	mcpSection := make(map[string]any)

	// Preserve existing entries not managed by us
	if existing, ok := doc["mcp"].(map[string]any); ok {
		for k, v := range existing {
			mcpSection[k] = v
		}
	}

	for name, srv := range cfg.MCPServers {
		if !srv.Enabled {
			continue
		}
		cmd := []string{srv.Command}
		cmd = append(cmd, srv.Args...)
		entry := map[string]any{
			"type":    "local",
			"command": cmd,
			"enabled": true,
		}
		mcpSection[name] = entry
	}
	doc["mcp"] = mcpSection

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}

// Codex TOML format with [mcp_servers.NAME] sections
func (m *Manager) proposedTOMLCodex(current string) (string, error) {
	cfg := m.store.Get()

	// Remove existing [mcp_servers.*] sections from current
	base := current
	if base != "" {
		re := regexp.MustCompile(`(?m)^\[mcp_servers\.[^\]]+\]\n(?:[^\[]*\n)*`)
		base = re.ReplaceAllString(base, "")
		base = strings.TrimRight(base, "\n\r\t ")
	}

	// Generate new [mcp_servers.*] sections
	var sb strings.Builder
	if base != "" {
		sb.WriteString(base)
		sb.WriteString("\n\n")
	}

	for name, srv := range cfg.MCPServers {
		if !srv.Enabled {
			continue
		}
		sb.WriteString(fmt.Sprintf("[mcp_servers.%s]\n", name))
		sb.WriteString(fmt.Sprintf("command = %q\n", srv.Command))

		// Format args as TOML array
		sb.WriteString("args = [ ")
		for i, arg := range srv.Args {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString(fmt.Sprintf("%q", arg))
		}
		sb.WriteString(" ]\n")

		if len(srv.Env) > 0 {
			sb.WriteString("[mcp_servers.")
			sb.WriteString(name)
			sb.WriteString(".env]\n")
			for k, v := range srv.Env {
				sb.WriteString(fmt.Sprintf("%s = %q\n", k, v))
			}
		}

		sb.WriteString("\n")
	}

	return strings.TrimRight(sb.String(), "\n") + "\n", nil
}
