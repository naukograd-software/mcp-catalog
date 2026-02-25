package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/naukograd-software/mcp-catalog/internal/config"
)

const proxyProtocolVersion = "2024-11-05"
const proxyTimeout = 30 * time.Second

const proxyResourcePrefix = "mcp-catalog://resource/"
const proxyResourceTemplatePrefix = "mcp-catalog://resource-template/"

type mcpSession struct {
	Tools             map[string]toolRoute
	Prompts           map[string]promptRoute
	Resources         map[string]resourceRoute
	ResourceTemplates map[string]resourceRoute
}

type toolRoute struct {
	ServerName string
	ToolName   string
}

type promptRoute struct {
	ServerName string
	PromptName string
}

type resourceRoute struct {
	ServerName   string
	OriginalURI  string
	TemplateMode bool
}

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type proxiedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

type toolsListResult struct {
	Tools []proxiedTool `json:"tools"`
}

type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

func (s *Server) handleMCPProxy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodDelete:
		s.handleMCPDelete(w, r)
		return
	case http.MethodPost:
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req rpcReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.JSONRPC == "" {
		req.JSONRPC = "2.0"
	}

	sessionID := strings.TrimSpace(r.Header.Get("MCP-Session-Id"))
	switch req.Method {
	case "initialize":
		s.handleMCPInitialize(w, req)
		return
	case "notifications/initialized":
		if sessionID == "" || !s.hasSession(sessionID) {
			s.writeRPCError(w, req.ID, -32000, "missing or invalid MCP session")
			return
		}
		w.Header().Set("MCP-Session-Id", sessionID)
		w.WriteHeader(http.StatusNoContent)
		return
	case "tools/list":
		if sessionID == "" || !s.hasSession(sessionID) {
			s.writeRPCError(w, req.ID, -32000, "missing or invalid MCP session")
			return
		}
		tools, routes := s.aggregateTools()
		s.updateSessionTools(sessionID, routes)
		s.writeRPCResult(w, req.ID, toolsListResult{Tools: tools}, sessionID)
		return
	case "tools/call":
		if sessionID == "" || !s.hasSession(sessionID) {
			s.writeRPCError(w, req.ID, -32000, "missing or invalid MCP session")
			return
		}
		var params toolsCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			s.writeRPCError(w, req.ID, -32602, "invalid tools/call params")
			return
		}
		if params.Name == "" {
			s.writeRPCError(w, req.ID, -32602, "tools/call name is required")
			return
		}
		route, ok := s.resolveToolRoute(sessionID, params.Name)
		if !ok {
			s.writeRPCError(w, req.ID, -32601, "tool not found")
			return
		}
		result, err := s.callTool(route.ServerName, route.ToolName, params.Arguments)
		if err != nil {
			s.writeRPCError(w, req.ID, -32000, err.Error())
			return
		}
		s.writeRawResult(w, req.ID, result, sessionID)
		return
	case "prompts/list":
		if sessionID == "" || !s.hasSession(sessionID) {
			s.writeRPCError(w, req.ID, -32000, "missing or invalid MCP session")
			return
		}
		items, routes := s.aggregatePrompts()
		s.updateSessionPrompts(sessionID, routes)
		s.writeRPCResult(w, req.ID, map[string]any{"prompts": items}, sessionID)
		return
	case "prompts/get":
		if sessionID == "" || !s.hasSession(sessionID) {
			s.writeRPCError(w, req.ID, -32000, "missing or invalid MCP session")
			return
		}
		params := make(map[string]any)
		if err := json.Unmarshal(req.Params, &params); err != nil {
			s.writeRPCError(w, req.ID, -32602, "invalid prompts/get params")
			return
		}
		name, _ := params["name"].(string)
		if name == "" {
			s.writeRPCError(w, req.ID, -32602, "prompts/get name is required")
			return
		}
		route, ok := s.resolvePromptRoute(sessionID, name)
		if !ok {
			s.writeRPCError(w, req.ID, -32601, "prompt not found")
			return
		}
		params["name"] = route.PromptName
		result, err := s.forwardPromptGet(route.ServerName, params)
		if err != nil {
			s.writeRPCError(w, req.ID, -32000, err.Error())
			return
		}
		s.writeRawResult(w, req.ID, result, sessionID)
		return
	case "resources/list":
		if sessionID == "" || !s.hasSession(sessionID) {
			s.writeRPCError(w, req.ID, -32000, "missing or invalid MCP session")
			return
		}
		items, routes := s.aggregateResources()
		s.updateSessionResources(sessionID, routes)
		s.writeRPCResult(w, req.ID, map[string]any{"resources": items}, sessionID)
		return
	case "resources/templates/list":
		if sessionID == "" || !s.hasSession(sessionID) {
			s.writeRPCError(w, req.ID, -32000, "missing or invalid MCP session")
			return
		}
		items, routes := s.aggregateResourceTemplates()
		s.updateSessionResourceTemplates(sessionID, routes)
		s.writeRPCResult(w, req.ID, map[string]any{"resourceTemplates": items}, sessionID)
		return
	case "resources/read":
		if sessionID == "" || !s.hasSession(sessionID) {
			s.writeRPCError(w, req.ID, -32000, "missing or invalid MCP session")
			return
		}
		params := make(map[string]any)
		if err := json.Unmarshal(req.Params, &params); err != nil {
			s.writeRPCError(w, req.ID, -32602, "invalid resources/read params")
			return
		}
		uri, _ := params["uri"].(string)
		if uri == "" {
			s.writeRPCError(w, req.ID, -32602, "resources/read uri is required")
			return
		}
		route, ok := s.resolveResourceRoute(sessionID, uri)
		if !ok {
			s.writeRPCError(w, req.ID, -32601, "resource not found")
			return
		}
		params["uri"] = route.OriginalURI
		result, err := s.forwardResourceRead(route.ServerName, params)
		if err != nil {
			s.writeRPCError(w, req.ID, -32000, err.Error())
			return
		}
		s.writeRawResult(w, req.ID, result, sessionID)
		return
	default:
		s.writeRPCError(w, req.ID, -32601, "method not found")
		return
	}
}

func (s *Server) handleMCPInitialize(w http.ResponseWriter, req rpcReq) {
	sessionID, err := newSessionID()
	if err != nil {
		s.writeRPCError(w, req.ID, -32603, "failed to allocate session")
		return
	}
	s.mcpMu.Lock()
	s.mcpState[sessionID] = &mcpSession{
		Tools:             make(map[string]toolRoute),
		Prompts:           make(map[string]promptRoute),
		Resources:         make(map[string]resourceRoute),
		ResourceTemplates: make(map[string]resourceRoute),
	}
	s.mcpMu.Unlock()

	result := map[string]any{
		"protocolVersion": proxyProtocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{
				"listChanged": true,
			},
			"prompts": map[string]any{
				"listChanged": true,
			},
			"resources": map[string]any{
				"listChanged": true,
			},
		},
		"serverInfo": map[string]any{
			"name":    "mcp-catalog-proxy",
			"version": "1.0.0",
		},
	}
	s.writeRPCResult(w, req.ID, result, sessionID)
}

func (s *Server) handleMCPDelete(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.Header.Get("MCP-Session-Id"))
	if sessionID == "" {
		http.Error(w, "missing MCP-Session-Id", http.StatusBadRequest)
		return
	}
	s.mcpMu.Lock()
	delete(s.mcpState, sessionID)
	s.mcpMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) hasSession(sessionID string) bool {
	s.mcpMu.RLock()
	defer s.mcpMu.RUnlock()
	_, ok := s.mcpState[sessionID]
	return ok
}

func (s *Server) updateSessionTools(sessionID string, routes map[string]toolRoute) {
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	ss, ok := s.mcpState[sessionID]
	if !ok {
		return
	}
	ss.Tools = routes
}

func (s *Server) updateSessionPrompts(sessionID string, routes map[string]promptRoute) {
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	ss, ok := s.mcpState[sessionID]
	if !ok {
		return
	}
	ss.Prompts = routes
}

func (s *Server) updateSessionResources(sessionID string, routes map[string]resourceRoute) {
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	ss, ok := s.mcpState[sessionID]
	if !ok {
		return
	}
	ss.Resources = routes
}

func (s *Server) updateSessionResourceTemplates(sessionID string, routes map[string]resourceRoute) {
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	ss, ok := s.mcpState[sessionID]
	if !ok {
		return
	}
	ss.ResourceTemplates = routes
}

func (s *Server) resolveToolRoute(sessionID, tool string) (toolRoute, bool) {
	s.mcpMu.RLock()
	ss, ok := s.mcpState[sessionID]
	s.mcpMu.RUnlock()
	if ok {
		if r, ok := ss.Tools[tool]; ok {
			return r, true
		}
	}

	parts := strings.SplitN(tool, "__", 2)
	if len(parts) != 2 {
		return toolRoute{}, false
	}
	return toolRoute{ServerName: parts[0], ToolName: parts[1]}, true
}

func (s *Server) resolvePromptRoute(sessionID, name string) (promptRoute, bool) {
	s.mcpMu.RLock()
	ss, ok := s.mcpState[sessionID]
	s.mcpMu.RUnlock()
	if ok {
		if r, ok := ss.Prompts[name]; ok {
			return r, true
		}
	}

	parts := strings.SplitN(name, "__", 2)
	if len(parts) != 2 {
		return promptRoute{}, false
	}
	return promptRoute{ServerName: parts[0], PromptName: parts[1]}, true
}

func (s *Server) resolveResourceRoute(sessionID, uri string) (resourceRoute, bool) {
	s.mcpMu.RLock()
	ss, ok := s.mcpState[sessionID]
	s.mcpMu.RUnlock()
	if ok {
		if r, ok := ss.Resources[uri]; ok {
			return r, true
		}
		if r, ok := ss.ResourceTemplates[uri]; ok {
			return r, true
		}
	}

	if r, ok := parseProxyResourceURI(uri); ok {
		return r, true
	}
	return resourceRoute{}, false
}

func (s *Server) aggregateTools() ([]proxiedTool, map[string]toolRoute) {
	cfg := s.store.Get()
	tools := make([]proxiedTool, 0)
	routes := make(map[string]toolRoute)
	for serverName, srv := range cfg.MCPServers {
		if srv == nil || !srv.Enabled {
			continue
		}
		serverTools, err := s.listTools(serverName, srv)
		if err != nil {
			continue
		}
		for _, t := range serverTools {
			name := serverName + "__" + t.Name
			tools = append(tools, proxiedTool{
				Name:        name,
				Description: t.Description,
				InputSchema: t.InputSchema,
			})
			routes[name] = toolRoute{ServerName: serverName, ToolName: t.Name}
		}
	}
	return tools, routes
}

func (s *Server) aggregatePrompts() ([]map[string]any, map[string]promptRoute) {
	cfg := s.store.Get()
	items := make([]map[string]any, 0)
	routes := make(map[string]promptRoute)
	for serverName, srv := range cfg.MCPServers {
		if srv == nil || !srv.Enabled {
			continue
		}
		res, err := s.forwardMCP(serverName, srv, "prompts/list", map[string]any{})
		if err != nil {
			continue
		}
		prompts, err := parseListObjects(res, "prompts")
		if err != nil {
			continue
		}
		for _, p := range prompts {
			name, _ := p["name"].(string)
			if name == "" {
				continue
			}
			proxyName := serverName + "__" + name
			p["name"] = proxyName
			items = append(items, p)
			routes[proxyName] = promptRoute{ServerName: serverName, PromptName: name}
		}
	}
	return items, routes
}

func (s *Server) aggregateResources() ([]map[string]any, map[string]resourceRoute) {
	cfg := s.store.Get()
	items := make([]map[string]any, 0)
	routes := make(map[string]resourceRoute)
	for serverName, srv := range cfg.MCPServers {
		if srv == nil || !srv.Enabled {
			continue
		}
		res, err := s.forwardMCP(serverName, srv, "resources/list", map[string]any{})
		if err != nil {
			continue
		}
		resources, err := parseListObjects(res, "resources")
		if err != nil {
			continue
		}
		for _, r := range resources {
			uri, _ := r["uri"].(string)
			if uri == "" {
				continue
			}
			proxyURI := buildProxyResourceURI(serverName, uri, false)
			r["uri"] = proxyURI
			if name, _ := r["name"].(string); name != "" {
				r["name"] = serverName + " :: " + name
			}
			items = append(items, r)
			routes[proxyURI] = resourceRoute{ServerName: serverName, OriginalURI: uri}
		}
	}
	return items, routes
}

func (s *Server) aggregateResourceTemplates() ([]map[string]any, map[string]resourceRoute) {
	cfg := s.store.Get()
	items := make([]map[string]any, 0)
	routes := make(map[string]resourceRoute)
	for serverName, srv := range cfg.MCPServers {
		if srv == nil || !srv.Enabled {
			continue
		}
		res, err := s.forwardMCP(serverName, srv, "resources/templates/list", map[string]any{})
		if err != nil {
			continue
		}
		tpls, err := parseListObjects(res, "resourceTemplates")
		if err != nil {
			continue
		}
		for _, t := range tpls {
			uriTemplate, _ := t["uriTemplate"].(string)
			if uriTemplate == "" {
				continue
			}
			proxyURI := buildProxyResourceURI(serverName, uriTemplate, true)
			t["uriTemplate"] = proxyURI
			if name, _ := t["name"].(string); name != "" {
				t["name"] = serverName + " :: " + name
			}
			items = append(items, t)
			routes[proxyURI] = resourceRoute{ServerName: serverName, OriginalURI: uriTemplate, TemplateMode: true}
		}
	}
	return items, routes
}

func (s *Server) listTools(serverName string, srv *config.MCPServer) ([]proxiedTool, error) {
	res, err := s.forwardMCP(serverName, srv, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Tools []proxiedTool `json:"tools"`
	}
	if err := json.Unmarshal(res, &parsed); err != nil {
		return nil, err
	}
	return parsed.Tools, nil
}

func (s *Server) callTool(serverName, toolName string, args json.RawMessage) (json.RawMessage, error) {
	srv, ok := s.store.GetServer(serverName)
	if !ok {
		return nil, fmt.Errorf("server %q not found", serverName)
	}

	var parsedArgs any = map[string]any{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &parsedArgs); err != nil {
			return nil, fmt.Errorf("invalid tool arguments: %w", err)
		}
	}

	params := map[string]any{
		"name":      toolName,
		"arguments": parsedArgs,
	}
	return s.forwardMCP(serverName, srv, "tools/call", params)
}

func (s *Server) forwardPromptGet(serverName string, params map[string]any) (json.RawMessage, error) {
	srv, ok := s.store.GetServer(serverName)
	if !ok {
		return nil, fmt.Errorf("server %q not found", serverName)
	}
	return s.forwardMCP(serverName, srv, "prompts/get", params)
}

func (s *Server) forwardResourceRead(serverName string, params map[string]any) (json.RawMessage, error) {
	srv, ok := s.store.GetServer(serverName)
	if !ok {
		return nil, fmt.Errorf("server %q not found", serverName)
	}
	return s.forwardMCP(serverName, srv, "resources/read", params)
}

func (s *Server) forwardMCP(serverName string, srv *config.MCPServer, method string, params any) (json.RawMessage, error) {
	_ = serverName
	ctx, cancel := context.WithTimeout(context.Background(), proxyTimeout)
	defer cancel()
	if strings.EqualFold(strings.TrimSpace(srv.Type), "streamableHttp") || (strings.TrimSpace(srv.URL) != "" && strings.TrimSpace(srv.Command) == "") {
		return forwardHTTP(ctx, srv, method, params)
	}
	return forwardStdio(ctx, srv, method, params)
}

func forwardHTTP(ctx context.Context, srv *config.MCPServer, method string, params any) (json.RawMessage, error) {
	url := strings.TrimSpace(srv.URL)
	if url == "" {
		return nil, fmt.Errorf("missing url")
	}
	client := &http.Client{Timeout: proxyTimeout}
	sessionID := ""

	send := func(payload map[string]any, expect bool, expectedID int) (*rpcResp, error) {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if sessionID != "" {
			req.Header.Set("MCP-Session-Id", sessionID)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if sid := strings.TrimSpace(resp.Header.Get("MCP-Session-Id")); sid != "" {
			sessionID = sid
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("http status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		if !expect {
			return nil, nil
		}
		return decodeProxyResponse(raw, expectedID)
	}

	closeSession := func() {
		if sessionID == "" {
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
		if err != nil {
			return
		}
		req.Header.Set("MCP-Session-Id", sessionID)
		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}
	defer closeSession()

	initReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": proxyProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "mcp-catalog-proxy",
				"version": "1.0.0",
			},
		},
	}
	initResp, err := send(initReq, true, 1)
	if err != nil {
		return nil, fmt.Errorf("initialize request: %w", err)
	}
	if initResp.Error != nil {
		return nil, fmt.Errorf("initialize: %s", initResp.Error.Message)
	}

	if _, err := send(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}, false, 0); err != nil {
		// non-fatal
	}

	callReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  method,
		"params":  params,
	}
	callResp, err := send(callReq, true, 2)
	if err != nil {
		return nil, err
	}
	if callResp.Error != nil {
		return nil, fmt.Errorf("%s: %s", method, callResp.Error.Message)
	}
	return callResp.Result, nil
}

func forwardStdio(ctx context.Context, srv *config.MCPServer, method string, params any) (json.RawMessage, error) {
	command := strings.TrimSpace(srv.Command)
	if command == "" {
		return nil, fmt.Errorf("missing command")
	}
	cmd := exec.CommandContext(ctx, command, srv.Args...)
	if len(srv.Env) > 0 {
		env := cmd.Environ()
		for k, v := range srv.Env {
			env = append(env, fmt.Sprintf("%s=%s", k, v))
		}
		cmd.Env = env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	go io.Copy(io.Discard, stderrPipe)

	stdout := bufio.NewReader(stdoutPipe)
	writeReq := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = stdin.Write(append(b, '\n'))
		return err
	}
	readResp := func() (*rpcResp, error) {
		line, err := stdout.ReadString('\n')
		if err != nil {
			return nil, err
		}
		var resp rpcResp
		if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &resp); err != nil {
			return nil, err
		}
		return &resp, nil
	}

	if err := writeReq(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": proxyProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "mcp-catalog-proxy",
				"version": "1.0.0",
			},
		},
	}); err != nil {
		return nil, err
	}
	initResp, err := readResp()
	if err != nil {
		return nil, err
	}
	if initResp.Error != nil {
		return nil, fmt.Errorf("initialize: %s", initResp.Error.Message)
	}

	_ = writeReq(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	if err := writeReq(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  method,
		"params":  params,
	}); err != nil {
		return nil, err
	}
	callResp, err := readResp()
	if err != nil {
		return nil, err
	}
	if callResp.Error != nil {
		return nil, fmt.Errorf("%s: %s", method, callResp.Error.Message)
	}

	if len(callResp.Result) == 0 {
		return json.RawMessage(`{}`), nil
	}
	return callResp.Result, nil
}

func decodeProxyResponse(raw []byte, expectedID int) (*rpcResp, error) {
	data := strings.TrimSpace(string(raw))
	if data == "" {
		return nil, fmt.Errorf("empty response body")
	}
	var candidates []rpcResp
	add := func(v rpcResp) {
		if v.JSONRPC == "" && v.Result == nil && v.Error == nil {
			return
		}
		candidates = append(candidates, v)
	}

	var one rpcResp
	if err := json.Unmarshal([]byte(data), &one); err == nil {
		add(one)
	}
	var arr []rpcResp
	if err := json.Unmarshal([]byte(data), &arr); err == nil {
		for _, v := range arr {
			add(v)
		}
	}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var sseOne rpcResp
		if err := json.Unmarshal([]byte(payload), &sseOne); err == nil {
			add(sseOne)
			continue
		}
		var sseArr []rpcResp
		if err := json.Unmarshal([]byte(payload), &sseArr); err == nil {
			for _, v := range sseArr {
				add(v)
			}
		}
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("unable to decode response: %s", data)
	}
	if expectedID > 0 {
		for i := range candidates {
			if candidates[i].ID == expectedID {
				return &candidates[i], nil
			}
		}
		return nil, fmt.Errorf("response id=%d not found", expectedID)
	}
	return &candidates[0], nil
}

func parseListObjects(raw json.RawMessage, key string) ([]map[string]any, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	listRaw, ok := payload[key]
	if !ok {
		return []map[string]any{}, nil
	}
	var items []map[string]any
	if err := json.Unmarshal(listRaw, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func buildProxyResourceURI(serverName, originalURI string, template bool) string {
	encoded := hex.EncodeToString([]byte(originalURI))
	if template {
		return proxyResourceTemplatePrefix + serverName + "/" + encoded
	}
	return proxyResourcePrefix + serverName + "/" + encoded
}

func parseProxyResourceURI(uri string) (resourceRoute, bool) {
	prefix := proxyResourcePrefix
	template := false
	if strings.HasPrefix(uri, proxyResourceTemplatePrefix) {
		prefix = proxyResourceTemplatePrefix
		template = true
	} else if !strings.HasPrefix(uri, proxyResourcePrefix) {
		return resourceRoute{}, false
	}
	value := strings.TrimPrefix(uri, prefix)
	parts := strings.SplitN(value, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return resourceRoute{}, false
	}
	decoded, err := hex.DecodeString(parts[1])
	if err != nil {
		return resourceRoute{}, false
	}
	return resourceRoute{ServerName: parts[0], OriginalURI: string(decoded), TemplateMode: template}, true
}

func (s *Server) writeRPCResult(w http.ResponseWriter, id int, result any, sessionID string) {
	raw, err := json.Marshal(result)
	if err != nil {
		s.writeRPCError(w, id, -32603, "failed to encode result")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if sessionID != "" {
		w.Header().Set("MCP-Session-Id", sessionID)
	}
	_ = json.NewEncoder(w).Encode(rpcResp{JSONRPC: "2.0", ID: id, Result: raw})
}

func (s *Server) writeRawResult(w http.ResponseWriter, id int, result json.RawMessage, sessionID string) {
	w.Header().Set("Content-Type", "application/json")
	if sessionID != "" {
		w.Header().Set("MCP-Session-Id", sessionID)
	}
	if len(result) == 0 {
		result = json.RawMessage(`{}`)
	}
	_ = json.NewEncoder(w).Encode(rpcResp{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) writeRPCError(w http.ResponseWriter, id int, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rpcResp{JSONRPC: "2.0", ID: id, Error: &rpcErr{Code: code, Message: msg}})
}

func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
