//go:build linux || darwin

package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/protocol"
	"github.com/viewlegacy/onprest/internal/ws"
)

// The production registry stays closed to file; this explicitly prepared
// internal provider uses the real signed bidirectional WebSocket connection and private store.
func TestFileProviderRealSignedInputSaveAndIndependentOutput(t *testing.T) {
	for _, written := range []bool{false, true} {
		t.Run(map[bool]string{false: "save_only", true: "written"}[written], func(t *testing.T) {
			executor := `{type: file, write: [{input: /document, source: exchange, name: saved.bin}]`
			output := ""
			if written {
				executor += `, read: {written: true}`
				output = "    output: {files: {}}\n"
			}
			f := fileController(t, executor+"}", output, "")
			accepted := make(chan *ws.Conn, 1)
			closed := make(chan struct{})
			private, _ := base64.RawURLEncoding.DecodeString(testAgentPrivateKey)
			public := ed25519.PrivateKey(private).Public().(ed25519.PublicKey)
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/ws/agent/challenge" {
					w.WriteHeader(201)
					io.WriteString(w, `{"challenge":"file-provider-challenge"}`)
					return
				}
				signature, e := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Agent-Signature"))
				if e != nil || !ed25519.Verify(public, protocol.AgentAuthMessage(r.URL.Path, r.Header.Get("X-Agent-Timestamp"), r.Header.Get("X-Agent-Nonce"), r.Header.Get("X-Agent-Challenge"), r.Header.Get("Sec-WebSocket-Key")), signature) {
					http.Error(w, "invalid signature", 401)
					return
				}
				w.Header().Set(protocol.MajorHeader, protocol.Major)
				conn, e := ws.Accept(w, r)
				if e != nil {
					return
				}
				accepted <- conn
				<-closed
				conn.Close()
			}))
			f.cf.Gateway.URL = "ws" + strings.TrimPrefix(peer.URL, "http") + "/ws/agent"
			r := &Runner{cfg: Config{CapabilityFile: f.cf.ConfigPath, ReconnectEvery: 10 * time.Millisecond}, cf: f.cf, versioned: f.prepared, controller: f.ctl, store: f.store, controls: newControlAdmission(f.prepared.Runtime())}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- r.Run(ctx) }()
			var conn *ws.Conn
			select {
			case conn = <-accepted:
			case <-time.After(3 * time.Second):
				cancel()
				close(closed)
				peer.Close()
				t.Fatal("signed connection failed")
			}
			defer func() {
				cancel()
				conn.Close()
				close(closed)
				peer.Close()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("shutdown blocked")
				}
			}()
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			inputBegin(t, r, conn, "file-request", time.Now().Add(time.Second))
			ready := inputFile(t, conn, "file-request", "input-body", "/document", true)
			if _, e := os.Stat(filepath.Join(f.base, "exchange", "saved.bin")); !os.IsNotExist(e) {
				t.Fatal("executed before commit")
			}
			conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "request_commit", ID: "file-request", Params: map[string]any{"document": nil}, Files: []protocol.FileBinding{{Pointer: "/document", FileID: ready.FileID}}}))
			raw, e := conn.ReadText()
			var response protocol.Response
			var result contract.ResultValue
			if e != nil || json.Unmarshal(raw, &response) != nil || response.Error != nil || json.Unmarshal(response.Result, &result) != nil {
				t.Fatal(string(raw), e)
			}
			b, e := os.ReadFile(filepath.Join(f.base, "exchange", "saved.bin"))
			if e != nil || string(b) != "A" {
				t.Fatal("saved content", e, string(b))
			}
			if result.Data != nil {
				t.Fatal("unexpected data")
			}
			if !written {
				if len(result.Files) != 0 {
					t.Fatal("save-only files")
				}
				waitTransport(t, func() bool { return f.store.Usage() == 0 })
				return
			}
			if len(result.Files) != 1 || result.Files[0].Name != "saved.bin" || result.Files[0].MIMEType != "application/octet-stream" || result.Files[0].SizeBytes != 1 {
				t.Fatal("written metadata", result.Files)
			}
			// Input ownership has ended. A new native business writer cannot mutate the
			// already owned output snapshot, and the output remains transferable.
			waitTransport(t, func() bool { return f.store.Usage() == 1 })
			if e = os.WriteFile(filepath.Join(f.base, "exchange", "saved.bin"), []byte("later business version"), 0600); e != nil {
				t.Fatal(e)
			}
			conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "transfer_open", ID: "file-request", TransferID: "output-body", FileID: result.Files[0].ID}))
			chunk := inputRead(t, conn, "transfer_chunk")
			body, e := base64.StdEncoding.DecodeString(chunk.DataBase64)
			if e != nil || string(body) != "A" {
				t.Fatal("output lifetime/bytes", e, string(body))
			}
			conn.WriteText(protocol.MustJSON(protocol.Frame{Type: "ack", ID: "file-request", TransferID: "output-body", Sequence: chunk.Sequence}))
			end := inputRead(t, conn, "transfer_end")
			if end.Size != 1 || end.SHA256 != result.Files[0].SHA256 {
				t.Fatal("output integrity")
			}
		})
	}
}
