package deej

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

//go:embed web/*
var webAssets embed.FS

const (
	defaultServerPort     = 9123
	serverShutdownTimeout = 5 * time.Second
)

// Server provides an HTTP server for the web-based configuration UI
type Server struct {
	logger     *zap.SugaredLogger
	httpServer *http.Server
	port       int

	deej *Deej

	lock    sync.Mutex
	running bool
}

// NewServer creates a new web server instance
func NewServer(logger *zap.SugaredLogger, deej *Deej) *Server {
	return &Server{
		logger: logger.Named("server"),
		port:   defaultServerPort,
		deej:   deej,
	}
}

// Start begins serving the web UI
func (s *Server) Start() error {
	s.lock.Lock()
	defer s.lock.Unlock()

	if s.running {
		return fmt.Errorf("server already running")
	}

	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("/api/sliders", s.handleSliders)
	mux.HandleFunc("/api/sliders/", s.handleSliderByID)
	mux.HandleFunc("/api/sessions", s.handleSessions)
	mux.HandleFunc("/api/status", s.handleStatus)

	// Static files - serve embedded SPA
	staticFS, err := fs.Sub(webAssets, "web")
	if err != nil {
		return fmt.Errorf("get static fs: %w", err)
	}
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	// Wrap with middleware
	handler := s.corsMiddleware(s.loggingMiddleware(mux))

	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: handler,
	}

	listener, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", s.port, err)
	}

	s.running = true
	s.logger.Infow("Web server started",
		"port", s.port,
		"url", fmt.Sprintf("http://localhost:%d", s.port))

	go func() {
		if err := s.httpServer.Serve(listener); err != http.ErrServerClosed {
			s.logger.Errorw("Server error", "error", err)
		}
	}()

	return nil
}

// Stop gracefully shuts down the server
func (s *Server) Stop() error {
	s.lock.Lock()
	defer s.lock.Unlock()

	if !s.running {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
	defer cancel()

	if err := s.httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown server: %w", err)
	}

	s.running = false
	s.logger.Info("Web server stopped")
	return nil
}

// GetURL returns the server URL
func (s *Server) GetURL() string {
	return fmt.Sprintf("http://localhost:%d", s.port)
}

// Middleware

func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(wrapped, r)

		s.logger.Debugw("HTTP request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.statusCode,
			"duration", time.Since(start))
	})
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// API Handlers

type slidersResponse struct {
	Sliders map[string][]string `json:"sliders"`
}

type sessionsResponse struct {
	Sessions []SessionInfo `json:"sessions"`
}

type updateSliderRequest struct {
	Apps []string `json:"apps"`
}

type genericResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

type statusResponse struct {
	Status      string `json:"status"`
	SliderCount int    `json:"sliderCount"`
	WebURL      string `json:"webUrl"`
}

func (s *Server) handleSliders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rawMapping := s.deej.config.GetSliderMappingRaw()

	// Convert int keys to string keys for JSON
	sliders := make(map[string][]string)
	for k, v := range rawMapping {
		sliders[strconv.Itoa(k)] = v
	}

	s.writeJSON(w, slidersResponse{Sliders: sliders})
}

func (s *Server) handleSliderByID(w http.ResponseWriter, r *http.Request) {
	// Extract slider ID from path: /api/sliders/0
	path := strings.TrimPrefix(r.URL.Path, "/api/sliders/")
	sliderID, err := strconv.Atoi(path)
	if err != nil {
		http.Error(w, "Invalid slider ID", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		rawMapping := s.deej.config.GetSliderMappingRaw()
		apps, ok := rawMapping[sliderID]
		if !ok {
			apps = []string{}
		}
		s.writeJSON(w, map[string][]string{"apps": apps})

	case http.MethodPut:
		var req updateSliderRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
			return
		}

		// Get current mapping, update the specific slider, write back
		currentMapping := s.deej.config.GetSliderMappingRaw()
		currentMapping[sliderID] = req.Apps

		if err := s.deej.config.WriteSliderMapping(currentMapping); err != nil {
			s.logger.Errorw("Failed to write config", "error", err)
			s.writeJSON(w, genericResponse{
				Success: false,
				Message: "Failed to save configuration",
			})
			return
		}

		s.writeJSON(w, genericResponse{
			Success: true,
			Message: "Slider updated - config will auto-reload",
		})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessions := s.deej.sessions.GetAllSessionKeys()
	s.writeJSON(w, sessionsResponse{Sessions: sessions})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rawMapping := s.deej.config.GetSliderMappingRaw()

	s.writeJSON(w, statusResponse{
		Status:      "running",
		SliderCount: len(rawMapping),
		WebURL:      s.GetURL(),
	})
}

func (s *Server) writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		s.logger.Errorw("Failed to encode JSON response", "error", err)
	}
}
