package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/naukograd-software/mcp-catalog/internal/config"
	"github.com/naukograd-software/mcp-catalog/internal/manager"
)

//go:embed all:static
var staticFiles embed.FS

type Server struct {
	store    *config.Store
	mgr      *manager.Manager
	clients  map[*websocket.Conn]bool
	mu       sync.RWMutex
	upgrader websocket.Upgrader
}

func New(store *config.Store, mgr *manager.Manager) *Server {
	s := &Server{
		store:   store,
		mgr:     mgr,
		clients: make(map[*websocket.Conn]bool),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}

	// Subscribe to manager events
	mgr.OnChange(func(name string, info *manager.ServerInfo) {
		s.broadcast(map[string]interface{}{
			"type":   "server_update",
			"name":   name,
			"server": info,
		})
	})

	return s
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("/api/servers", s.handleServers)
	mux.HandleFunc("/api/servers/", s.handleServer)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/config/export", s.handleExport)
	mux.HandleFunc("/api/config/import", s.handleImport)
	mux.HandleFunc("/api/apply/", s.handleApply)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/ws", s.handleWS)

	// Static files
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	return recoveryMiddleware(mux)
}

func recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("panic recovered: %v [%s %s]", err, r.Method, r.URL.Path)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// GET /api/servers - list all servers with status
func (s *Server) handleServers(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "method not allowed", 405)
		return
	}

	info := s.mgr.GetAllInfo()
	writeJSON(w, info)
}

// /api/servers/{name} - manage a specific server
func (s *Server) handleServer(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/servers/")
	parts := strings.SplitN(name, "/", 2)
	name = parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch r.Method {
	case "GET":
		info, ok := s.mgr.GetInfo(name)
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		writeJSON(w, info)

	case "PUT":
		// Add or update server
		var srv config.MCPServer
		if err := json.NewDecoder(r.Body).Decode(&srv); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := s.store.AddServer(name, &srv); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if srv.Enabled {
			go s.mgr.Check(name)
		}
		writeJSON(w, map[string]string{"status": "ok"})

	case "DELETE":
		s.mgr.RemoveServer(name)
		if err := s.store.RemoveServer(name); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})

	case "POST":
		switch action {
		case "check":
			go s.mgr.Check(name)
			writeJSON(w, map[string]string{"status": "ok"})
		default:
			http.Error(w, "unknown action", 400)
		}

	default:
		http.Error(w, "method not allowed", 405)
	}
}

// GET /api/config - get full config
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		cfg := s.store.Get()
		writeJSON(w, cfg)
	case "PUT":
		var cfg config.Config
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := s.store.Set(&cfg); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// GET /api/config/export
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	data, err := s.store.Export()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=mcp-servers.json")
	w.Write(data)
}

// POST /api/config/import
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	var cfg config.Config
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := s.store.Set(&cfg); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// GET /api/apply/{tool}
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	tool := strings.TrimPrefix(r.URL.Path, "/api/apply/")
	result, err := s.mgr.ApplyToCLI(tool)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, result)
}

// GET/PUT /api/settings
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		writeJSON(w, map[string]int{
			"healthCheckInterval": s.store.GetHealthCheckInterval(),
		})
	case "PUT":
		var body struct {
			HealthCheckInterval int `json:"healthCheckInterval"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := s.store.SetHealthCheckInterval(body.HealthCheckInterval); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.mgr.SetHealthInterval(body.HealthCheckInterval)
		writeJSON(w, map[string]string{"status": "ok"})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// WebSocket handler
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WS upgrade error: %v", err)
		return
	}

	s.mu.Lock()
	s.clients[conn] = true
	s.mu.Unlock()

	// Send initial state
	info := s.mgr.GetAllInfo()
	msg, _ := json.Marshal(map[string]interface{}{
		"type":    "initial",
		"servers": info,
	})
	conn.WriteMessage(websocket.TextMessage, msg)

	// Read loop (keep alive)
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}

	s.mu.Lock()
	delete(s.clients, conn)
	s.mu.Unlock()
	conn.Close()
}

func (s *Server) broadcast(data interface{}) {
	msg, err := json.Marshal(data)
	if err != nil {
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	for conn := range s.clients {
		if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			conn.Close()
			go func(c *websocket.Conn) {
				s.mu.Lock()
				delete(s.clients, c)
				s.mu.Unlock()
			}(conn)
		}
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
