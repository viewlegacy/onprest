package agent

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
)

const timeoutDetailTestDriverName = "onprest_timeout_detail_test"

func init() {
	sql.Register(timeoutDetailTestDriverName, timeoutDetailTestDriver{})
}

type timeoutDetailTestDriver struct{}

func (timeoutDetailTestDriver) Open(string) (driver.Conn, error) {
	return timeoutDetailTestConn{}, nil
}

type timeoutDetailTestConn struct{}

func (timeoutDetailTestConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (timeoutDetailTestConn) Close() error {
	return nil
}

func (timeoutDetailTestConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}

func (timeoutDetailTestConn) QueryContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	<-ctx.Done()
	return nil, errors.New("driver detail leaked table customers column email")
}

func TestRunnerWritesFixedTimeoutDetailToDetailLog(t *testing.T) {
	db, err := sql.Open(timeoutDetailTestDriverName, "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	cf := validCapabilityFile()
	cap := cf.Capabilities["get_customer"]
	cap.Policy.Timeout = "1ms"
	cf.Capabilities["get_customer"] = cap

	detail := &strings.Builder{}
	r := &Runner{
		caps:      cf.ByName(),
		cf:        cf,
		db:        db,
		detailLog: detail,
	}

	resp := r.handle(t.Context(), wireRequest{
		ID:         "req-timeout",
		Capability: "get_customer",
		Params:     map[string]any{"id": int64(1)},
	})
	if resp.Error == nil || resp.Error.Code != "AGENT_QUERY_TIMEOUT" {
		t.Fatalf("response = %#v", resp)
	}
	logs := detail.String()
	for _, want := range []string{"AGENT_QUERY_TIMEOUT", queryTimeoutDetail} {
		if !strings.Contains(logs, want) {
			t.Fatalf("detail log missing %q: %s", want, logs)
		}
	}
	for _, leaked := range []string{"customers", "email", "driver detail"} {
		if strings.Contains(logs, leaked) || strings.Contains(resp.Error.Detail, leaked) {
			t.Fatalf("timeout leaked driver detail %q: response=%#v logs=%s", leaked, resp.Error, logs)
		}
	}
}
