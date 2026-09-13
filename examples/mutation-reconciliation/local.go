// Local helpers for this example; use the separately distributed Gateway and Agent binaries.
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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func main() {
	var err error
	switch {
	case len(os.Args) == 5 && os.Args[1] == "configure":
		err = configure(os.Args[2], os.Args[3], os.Args[4])
	case len(os.Args) == 2 && os.Args[1] == "lose-response":
		err = loseResponse()
	default:
		err = errors.New("usage: go run ./local.go configure postgres|mysql|sqlserver|oracle WORK_DIR BIN_DIR\n       go run ./local.go lose-response")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func configure(db, workDir, binDir string) error {
	switch db {
	case "postgres", "mysql", "sqlserver", "oracle":
	default:
		return fmt.Errorf("unknown database: %s", db)
	}
	var err error
	workDir, err = filepath.Abs(workDir)
	if err != nil {
		return err
	}
	binDir, err = filepath.Abs(binDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(workDir, 0700); err != nil {
		return err
	}
	names := []string{"capability.yaml", "gateway.env", "request.env"}
	for _, name := range names {
		if _, err := os.Lstat(filepath.Join(workDir, name)); !os.IsNotExist(err) {
			return fmt.Errorf("use a fresh working directory; %s already exists or cannot be checked", name)
		}
	}
	template, err := os.ReadFile("capability." + db + ".yaml.tmpl")
	if err != nil {
		return err
	}
	var keys struct {
		Private string `json:"agent_private_key"`
		Public  string `json:"agent_public_key"`
	}
	var api struct {
		Key  string `json:"api_key"`
		Hash string `json:"key_hash"`
	}
	gateway := filepath.Join(binDir, "onprest-gateway")
	if err := commandJSON(gateway, &keys, "create-agent-secret"); err != nil {
		return err
	}
	if err := commandJSON(gateway, &api, "create-key", "--name", "reconciliation", "--capabilities", "create_order,reconcile_order"); err != nil {
		return err
	}
	if keys.Private == "" || keys.Public == "" || api.Key == "" || api.Hash == "" {
		return errors.New("key generation omitted required fields")
	}
	config := strings.ReplaceAll(string(template), "url: wss://replace-me:443/ws/agent", "url: ws://127.0.0.1:58080/ws/agent")
	config = strings.ReplaceAll(config, "agent_private_key: replace-me", "agent_private_key: "+keys.Private)
	if strings.Contains(config, "replace-me") {
		return errors.New("unfilled configuration placeholder")
	}
	records, err := json.Marshal([]any{map[string]any{"name": "reconciliation", "key_hash": api.Hash, "capabilities": []string{"create_order", "reconcile_order"}}})
	if err != nil {
		return err
	}
	common := "ONPREST_BIN_DIR=" + shellQuote(binDir) + "\nEXAMPLE_WORK_DIR=" + shellQuote(workDir) + "\n"
	files := []string{
		config,
		common + "GATEWAY_ADDR=127.0.0.1:58080\nGATEWAY_AGENT_PUBLIC_KEY=" + shellQuote(keys.Public) + "\nGATEWAY_API_KEYS_JSON=" + shellQuote(string(records)) + "\n",
		"GATEWAY_URL=http://127.0.0.1:58080\nONPREST_API_KEY=" + shellQuote(api.Key) + "\n",
	}
	for i, name := range names {
		f, err := os.OpenFile(filepath.Join(workDir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, writeErr := f.WriteString(files[i])
		closeErr := f.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	cmd := exec.Command(filepath.Join(binDir, "onprest-agent"), "validate", "--config", filepath.Join(workDir, "capability.yaml"))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("validate generated config: %w", err)
	}
	fmt.Printf("Configuration validated in %s\nGateway and Agent use gateway.env; curl requests use request.env.\n", workDir)
	return nil
}

func commandJSON(binary string, result any, args ...string) error {
	out, err := exec.Command(binary, args...).Output()
	if err != nil {
		return fmt.Errorf("%s %s failed: %w", filepath.Base(binary), args[0], err)
	}
	if err := json.Unmarshal(out, result); err != nil {
		return fmt.Errorf("decode %s output: %w", args[0], err)
	}
	return nil
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
