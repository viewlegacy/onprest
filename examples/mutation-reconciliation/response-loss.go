// Discard one successful INSERT response for the local reconciliation example.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

func main() {
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: go run ./examples/mutation-reconciliation/response-loss.go")
		os.Exit(2)
	}
	if err := loseResponse(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Consume a successful upstream mutation response, then close the downstream
// socket without sending that response. This demonstrates caller-side loss
// after completion, not an Agent disconnect or an unfinished transaction.
func loseResponse() error {
	listener, err := net.Listen("tcp", "127.0.0.1:58081")
	if err != nil {
		return err
	}
	defer listener.Close()
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var mu sync.Mutex
	used := false
	failed := false
	finished := make(chan struct{})
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 40 * time.Second}
	stop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			server.Close()
		}
		close(finished)
	}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			w.Write([]byte("ready\n"))
			return
		}
		if r.Method != http.MethodPost || (r.URL.Path != "/api/v1/capabilities/create_order" && r.URL.Path != "/mcp") || r.URL.RawQuery != "" {
			http.Error(w, "use the example INSERT endpoint", http.StatusBadRequest)
			return
		}
		mu.Lock()
		if used {
			mu.Unlock()
			http.Error(w, "one attempt only; start a new helper for another request", http.StatusConflict)
			return
		}
		used = true
		mu.Unlock()
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
		if err == nil && r.URL.Path == "/mcp" {
			var call struct {
				Method string `json:"method"`
				Params struct {
					Name string `json:"name"`
				} `json:"params"`
			}
			err = json.Unmarshal(body, &call)
			if err == nil && (call.Method != "tools/call" || call.Params.Name != "create_order") {
				err = errors.New("only create_order tools/call is supported")
			}
		}
		finishError := func(message string, status int) {
			mu.Lock()
			failed = true
			mu.Unlock()
			http.Error(w, message, status)
			go stop()
		}
		if err != nil {
			finishError("invalid INSERT request", http.StatusBadRequest)
			return
		}
		request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://127.0.0.1:58080"+r.URL.Path, strings.NewReader(string(body)))
		if err != nil {
			finishError("cannot create upstream request", http.StatusBadGateway)
			return
		}
		for _, name := range []string{"Authorization", "Content-Type", "Accept", "MCP-Protocol-Version"} {
			request.Header.Set(name, r.Header.Get(name))
		}
		response, err := client.Do(request)
		if err != nil {
			finishError("upstream request failed; check the order before another attempt", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
		var result struct {
			Count  int `json:"count"`
			Result struct {
				IsError    bool `json:"isError"`
				Structured struct {
					Count int `json:"count"`
				} `json:"structuredContent"`
			} `json:"result"`
			Error json.RawMessage `json:"error"`
		}
		decodeErr := json.Unmarshal(data, &result)
		success := err == nil && decodeErr == nil && response.StatusCode == http.StatusOK && (result.Count == 1 || (r.URL.Path == "/mcp" && len(result.Error) == 0 && !result.Result.IsError && result.Result.Structured.Count == 1))
		if !success {
			mu.Lock()
			failed = true
			mu.Unlock()
			fmt.Fprintln(os.Stderr, "Upstream did not return a successful one-row INSERT; no response-loss simulation performed.")
			w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
			w.WriteHeader(response.StatusCode)
			w.Write(data)
			// Let the actual error response reach the caller before shutting down.
			go stop()
			return
		}
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			finishError("cannot close downstream connection", http.StatusInternalServerError)
			return
		}
		connection.Close()
		fmt.Println("Gateway returned a successful one-row INSERT. Closed the caller connection without its response. Read through port 58080 to confirm the order.")
		go stop()
	})
	fmt.Println("Ready on http://127.0.0.1:58081: the next INSERT success response will be discarded (one attempt, no retry).")
	err = server.Serve(listener)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-finished
	mu.Lock()
	defer mu.Unlock()
	if failed {
		return errors.New("response-loss simulation was not completed")
	}
	return nil
}
