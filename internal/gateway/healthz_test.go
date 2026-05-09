package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthzDoesNotRequireAPIKeyAndReportsAgentState(t *testing.T) {
	s := NewServer(Config{}, nil)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, req)
	assertHealthz(t, rec, false)

	s.agentMu.Lock()
	s.agent = &agentConn{}
	s.agentMu.Unlock()

	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec = httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, req)
	assertHealthz(t, rec, true)

	s.agentMu.Lock()
	s.agent = nil
	s.agentMu.Unlock()

	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec = httptest.NewRecorder()
	s.httpSrv.Handler.ServeHTTP(rec, req)
	assertHealthz(t, rec, false)
}

func assertHealthz(t *testing.T, rec *httptest.ResponseRecorder, wantConnected bool) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		OK             bool `json:"ok"`
		AgentConnected bool `json:"agent_connected"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode healthz: %v; body=%s", err, rec.Body.String())
	}
	if !body.OK || body.AgentConnected != wantConnected {
		t.Fatalf("healthz = %#v, want ok=true agent_connected=%t", body, wantConnected)
	}
}
