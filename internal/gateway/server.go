package gateway

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/viewlegacy/onprest/internal/protocol"
)

type Server struct {
	cfg             Config
	logOut          io.Writer
	logMu           sync.Mutex
	agentMu         sync.RWMutex
	agent           *agentConn
	authMu          sync.Mutex
	nonces          map[string]time.Time
	apiKeyCache     map[[sha256.Size]byte]cachedAPIKey
	openapi         map[string]any
	rateMu          sync.Mutex
	rate            map[string]*bucket
	rateLastCleanup time.Time
	httpSrv         *http.Server
}

type agentConn struct {
	conn     textConn
	pending  map[string]chan protocol.Response
	mu       sync.Mutex
	closed   bool
	send     chan agentWrite
	done     chan struct{}
	doneOnce sync.Once
}

type agentWrite struct {
	payload []byte
	result  chan error
	ctx     context.Context
}

type cachedAPIKey struct {
	key      APIKey
	expires  time.Time
	lastUsed time.Time
}

type textConn interface {
	ReadText() ([]byte, error)
	WriteText([]byte) error
	Close() error
}

type deadlineTextConn interface {
	textConn
	SetReadDeadline(time.Time) error
	SetWriteDeadline(time.Time) error
	WritePing([]byte) error
	SetPongHandler(func())
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewServer(cfg Config, logOut io.Writer) *Server {
	if cfg.AgentTimeout <= 0 {
		cfg.AgentTimeout = 30 * time.Second
	}
	if cfg.AgentWriteTimeout <= 0 {
		cfg.AgentWriteTimeout = 5 * time.Second
	}
	if cfg.AgentPingInterval <= 0 {
		cfg.AgentPingInterval = 15 * time.Second
	}
	if cfg.AgentPongTimeout <= 0 {
		cfg.AgentPongTimeout = 10 * time.Second
	}
	if cfg.BodyReadTimeout <= 0 {
		cfg.BodyReadTimeout = 15 * time.Second
	}
	if cfg.MaxRequestBodyBytes <= 0 {
		cfg.MaxRequestBodyBytes = defaultMaxRequestBodyBytes
	}
	s := &Server{cfg: cfg, logOut: logOut, rate: map[string]*bucket{}, nonces: map[string]time.Time{}, apiKeyCache: map[[sha256.Size]byte]cachedAPIKey{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/ws/agent", s.handleAgentWS)
	mux.HandleFunc("/api/v1/capabilities/", s.handleCapability)
	mux.HandleFunc("/openapi.json", s.handleOpenAPI)
	mux.HandleFunc("/mcp", s.handleMCP)
	s.httpSrv = &http.Server{Addr: cfg.Addr, Handler: s.withRecovery(s.withAccess(mux)), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	return s
}

func (s *Server) ListenAndServe() error {
	return s.ListenAndServeContext(context.Background())
}

func (s *Server) ListenAndServeContext(ctx context.Context) error {
	return serveWithShutdown(ctx, s.cfg.Addr, s.httpSrv.ListenAndServe, s.httpSrv.Shutdown, s.log)
}

func serveWithShutdown(ctx context.Context, addr string, serve func() error, shutdown func(context.Context) error, log func(string, map[string]any)) error {
	if log != nil {
		log("gateway_start", map[string]any{"addr": addr})
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- serve()
	}()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := shutdown(shutdownCtx); err != nil {
			return err
		}
		select {
		case err := <-errCh:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-shutdownCtx.Done():
			return shutdownCtx.Err()
		}
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "agent_connected": s.hasAgent()})
}

func (s *Server) hasAgent() bool {
	s.agentMu.RLock()
	defer s.agentMu.RUnlock()
	return s.agent != nil
}
