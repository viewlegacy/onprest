//go:build linux || darwin

package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
)

type fileRecoveryBrokenLog struct{ calls int }

func (w *fileRecoveryBrokenLog) Write([]byte) (int, error) {
	w.calls++
	return 0, errors.New("injected local diagnostic write failure")
}

func TestFileProviderStartupRecoveryWithoutCurrentFolders(t *testing.T) {
	if base := os.Getenv("ONPREST_FILE_RECOVERY_CHILD"); base != "" {
		dir, e := filesystem.OpenDirectory(filepath.Join(base, "old-business"))
		if e != nil {
			t.Fatal(e)
		}
		defer dir.Close()
		s, e := artifacts.Open(artifacts.Options{Directory: filepath.Join(base, "spool"), QuotaBytes: 8 << 20, MaxPending: 4, MaxObjects: 128, TransferTimeout: time.Minute})
		if e != nil {
			t.Fatal(e)
		}
		defer s.Close()
		body := []byte("new")
		hash := sha256.Sum256(body)
		v := contract.FileValue{RequestID: "previous-request", SizeBytes: int64(len(body)), SHA256: hex.EncodeToString(hash[:])}
		e = s.Save(context.Background(), v.RequestID, []artifacts.SaveFile{{Directory: dir, Name: "completed", Value: v}}, func(context.Context, contract.FileValue) (io.ReadCloser, error) {
			os.Stdout.WriteString("FILE_RECOVERY_BARRIER\n")
			time.Sleep(time.Hour)
			return io.NopCloser(bytes.NewReader(body)), nil
		}, nil)
		t.Fatal("unexpected child return", e)
	}
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "db_only_old_source_removed", true: "old_directory_unavailable"}[unavailable], func(t *testing.T) {
			base := t.TempDir()
			business := filepath.Join(base, "old-business")
			os.Mkdir(business, 0700)
			os.WriteFile(filepath.Join(business, "previous-completed"), []byte("retain"), 0600)
			cmd := exec.Command(os.Args[0], "-test.run=^TestFileProviderStartupRecoveryWithoutCurrentFolders$")
			cmd.Env = append(os.Environ(), "ONPREST_FILE_RECOVERY_CHILD="+base)
			pipe, e := cmd.StdoutPipe()
			if e != nil {
				t.Fatal(e)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if e = cmd.Start(); e != nil {
				t.Fatal(e)
			}
			ready := make(chan bool, 1)
			go func() {
				scan := bufio.NewScanner(pipe)
				for scan.Scan() {
					if scan.Text() == "FILE_RECOVERY_BARRIER" {
						ready <- true
						return
					}
				}
				ready <- false
			}()
			select {
			case ok := <-ready:
				if !ok {
					cmd.Process.Kill()
					cmd.Wait()
					t.Fatal(stderr.String())
				}
			case <-time.After(15 * time.Second):
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("child barrier timeout")
			}
			if e = cmd.Process.Kill(); e != nil {
				t.Fatal(e)
			}
			_ = cmd.Wait()
			orphan := filepath.Join(base, "spool", "objects", "i-00000000000000000000000000000000.spool")
			if e = os.WriteFile(orphan, []byte("orphan-input"), 0600); e != nil {
				t.Fatal(e)
			}
			if unavailable {
				if e = os.Rename(business, business+"-moved"); e != nil {
					t.Fatal(e)
				}
			}
			yaml := `schema_version: 1
service: {title: Current DB-only config, version: test}
gateway: {url: ws://localhost/ws/agent, agent_private_key: ` + testAgentPrivateKey + `}
runtime: {spool_dir: ` + filepath.Join(base, "spool") + `, spool_total_bytes: 8MiB, input_limits: {max_pending_requests: 4}}
sources: {databases: {db: {driver: postgres, host: localhost, port: 5432, name: test, user: test}}}
capabilities:
  run:
    executor: {type: db, source: db, sql: select 1}
    input: {properties: {}}
    output: {data: {}}
`
			cfg, e := contract.Compile([]byte(yaml))
			if e != nil {
				t.Fatal(e)
			}
			cf, e := capabilityRuntimeView(cfg, filepath.Join(base, "agent.yaml"))
			if e != nil {
				t.Fatal(e)
			}
			registry, _ := providers.NewRegistry(map[string]providers.Factory{"db": func(context.Context, *providers.Snapshot) (providers.Provider, error) {
				return &controllerFixture{}, nil
			}})
			prepared, e := registry.Prepare(context.Background(), cfg, cf.ConfigPath, providers.ResolveOptions{})
			if e != nil {
				t.Fatal(e)
			}
			defer prepared.Close()
			var detail bytes.Buffer
			r := &Runner{cfg: Config{CapabilityFile: cf.ConfigPath}, versioned: prepared, cf: cf, detailLog: &detail}
			e = r.initializeTransport()
			if unavailable {
				if !strings.Contains(detail.String(), "old-business") || !strings.Contains(detail.String(), "AGENT_STARTUP_FAILED") {
					t.Fatal("missing private recovery diagnostic", detail.String())
				}
				if e != nil && strings.Contains(e.Error(), base) {
					t.Fatal("private path in safe initialization error")
				}
				if e == nil {
					r.store.Close()
					t.Fatal("recovery admitted unavailable old directory")
				}
				if _, e = os.Stat(orphan); e != nil {
					t.Fatal("ephemeral deletion preceded failed recovery")
				}
				failedLog := &fileRecoveryBrokenLog{}
				r.detailLog = failedLog
				if e = r.initializeTransport(); e == nil || r.store != nil || failedLog.calls == 0 {
					t.Fatal("diagnostic failure admitted recovery", e, failedLog.calls)
				}
				if _, e = os.Stat(orphan); e != nil {
					t.Fatal("diagnostic failure permitted ephemeral cleanup")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			defer r.store.Close()
			if r.store.Usage() != 0 {
				t.Fatal("recovery quota not released")
			}
			if _, e = os.Stat(orphan); !os.IsNotExist(e) {
				t.Fatal("ordinary ephemeral cleanup not completed")
			}
			entries, e := os.ReadDir(business)
			if e != nil || len(entries) != 1 || entries[0].Name() != "previous-completed" {
				t.Fatal("temp recovery/completed preservation", entries, e)
			}
			b, e := os.ReadFile(filepath.Join(business, "previous-completed"))
			if e != nil || string(b) != "retain" {
				t.Fatal("completed content changed")
			}
			second, e := artifacts.Open(artifacts.Options{Directory: filepath.Join(base, "spool"), QuotaBytes: 8 << 20, MaxPending: 4, MaxObjects: 128, TransferTimeout: time.Minute})
			if e == nil {
				second.Close()
				t.Fatal("lifetime lock lost")
			}
		})
	}
}
