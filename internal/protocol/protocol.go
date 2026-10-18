package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/viewlegacy/onprest/internal/contract"
	"time"
)

const Major = "1"
const MajorHeader = "X-Onprest-Protocol-Major"

const AgentAuthVersion = "onprest-agent-v2"

const AgentVerifyAuthVersion = "onprest-agent-verify-v1"

type Request struct {
	Type           string         `json:"type"`
	CallerBinding  string         `json:"caller_binding,omitempty"`
	DefinitionHash string         `json:"definition_hash,omitempty"`
	Deadline       time.Time      `json:"deadline"`
	ID             string         `json:"id"`
	Capability     string         `json:"capability"`
	Params         map[string]any `json:"params"`
}

type CancelRequest struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type Response struct {
	Type    string          `json:"type"`
	Metrics *Metrics        `json:"metrics,omitempty"`
	ID      string          `json:"id"`
	Result  json.RawMessage `json:"-"`
	Error   *Error          `json:"error,omitempty"`
}

type Error = contract.ErrorValue

type Metrics struct {
	ExecutorKind   string `json:"executor_kind"`
	ReturnedRows   *int64 `json:"returned_rows,omitempty"`
	AffectedRows   *int64 `json:"affected_rows,omitempty"`
	FileCount      int64  `json:"file_count"`
	ResponseBytes  int64  `json:"response_bytes"`
	InputFileCount int64  `json:"input_file_count"`
	InputFileBytes int64  `json:"input_file_bytes"`
}

type OutputMetadata struct {
	REST            string         `json:"rest"`
	MCP             string         `json:"mcp"`
	MaxFileBytes    int64          `json:"max_file_bytes"`
	MaxTotalBytes   int64          `json:"max_total_bytes"`
	MaxFiles        int64          `json:"max_files"`
	InlineBytes     int64          `json:"inline_bytes,omitempty"`
	TTL             string         `json:"ttl"`
	TransferTimeout string         `json:"transfer_timeout"`
	MCPOutputSchema map[string]any `json:"mcp_output_schema"`
}
type CapabilityMetadata struct {
	Output           *OutputMetadata  `json:"output,omitempty"`
	DefinitionHash   string           `json:"definition_hash"`
	ExecutorKind     string           `json:"executor_kind"`
	ExecutionTimeout string           `json:"execution_timeout"`
	InputTimeout     string           `json:"input_timeout,omitempty"`
	EffectClass      string           `json:"effect_class"`
	InputSchema      map[string]any   `json:"input_schema"`
	InputLimits      map[string]int64 `json:"input_limits"`
}
type Metadata struct {
	Data          map[string]any                `json:"data"`
	Capabilities  map[string]CapabilityMetadata `json:"capabilities"`
	ResponseKinds map[string]string             `json:"response_kinds"`
}

func AgentAuthMessage(path, timestamp, nonce, challenge, handshakeKey string) []byte {
	return []byte(AgentAuthVersion + "\n" + path + "\n" + timestamp + "\n" + nonce + "\n" + challenge + "\n" + handshakeKey)
}

func AgentVerifyAuthMessage(path, timestamp, nonce, challenge string) []byte {
	return []byte(AgentVerifyAuthVersion + "\n" + path + "\n" + timestamp + "\n" + nonce + "\n" + challenge)
}

func ResultResponse(id string, result any) Response {
	if raw, ok := result.(json.RawMessage); ok {
		return Response{Type: "result", ID: id, Result: raw}
	}
	b, _ := json.Marshal(result)
	return Response{Type: "result", ID: id, Result: b}
}

// MustJSON encodes internal frames without HTML escaping, so "<", ">" and
// "&" keep their one-byte UTF-8 form and the wire size stays the input size.
func MustJSON(v any) []byte {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(v) != nil {
		return nil
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// JSON frames are explicit about their kind and carry data/files at the top
// level; Result is an in-process holder for the shared business result.
func (r Response) MarshalJSON() ([]byte, error) {
	m := map[string]any{"type": r.Type, "id": r.ID}
	if r.Type == "metadata" {
		m["metadata"] = json.RawMessage(r.Result)
	} else if r.Error != nil {
		m["type"] = "error"
		m["error"] = r.Error
		if r.Metrics != nil {
			m["metrics"] = r.Metrics
		}
	} else {
		var value map[string]json.RawMessage
		if e := json.Unmarshal(r.Result, &value); e != nil {
			return nil, e
		}
		m["data"] = value["data"]
		m["files"] = value["files"]
		m["metrics"] = r.Metrics
	}
	return json.Marshal(m)
}
func (r *Response) UnmarshalJSON(b []byte) error {
	value, e := contract.DecodeJSON(b, contract.Limits{MaxBytes: 16 << 20, MaxDepth: 64, MaxItems: 100000})
	if e != nil {
		return e
	}
	m, ok := value.(map[string]any)
	if !ok {
		return errors.New("response frame must be object")
	}
	*r = Response{}
	r.Type, _ = m["type"].(string)
	r.ID, _ = m["id"].(string)
	if r.ID == "" {
		return errors.New("response frame ID is required")
	}
	allowed := map[string]bool{"type": true, "id": true}
	switch r.Type {
	case "metadata":
		allowed["metadata"] = true
		if _, ok := m["metadata"].(map[string]any); !ok {
			return errors.New("metadata is required")
		}
		r.Result, e = json.Marshal(m["metadata"])
		if e == nil {
			var metadata Metadata
			e = decodeFixedFields(r.Result, &metadata)
		}
	case "result":
		for _, k := range []string{"data", "files", "metrics"} {
			allowed[k] = true
			if _, ok := m[k]; !ok {
				return errors.New("incomplete result frame")
			}
		}
		if _, ok := m["files"].([]any); !ok {
			return errors.New("files must be array")
		}
		if _, ok := m["metrics"].(map[string]any); !ok {
			return errors.New("metrics must be object")
		}
		r.Result, e = json.Marshal(map[string]any{"data": m["data"], "files": m["files"]})
		raw, _ := json.Marshal(m["metrics"])
		e = decodeFixedFields(raw, &r.Metrics)
	case "error":
		allowed["metrics"] = true
		if metrics, present := m["metrics"]; present {
			if _, ok := metrics.(map[string]any); !ok {
				return errors.New("metrics must be object")
			}
			raw, _ := json.Marshal(metrics)
			if e = decodeFixedFields(raw, &r.Metrics); e != nil {
				return e
			}
		}
		allowed["error"] = true
		if _, ok := m["error"].(map[string]any); !ok {
			return errors.New("error is required")
		}
		raw, _ := json.Marshal(m["error"])
		e = decodeFixedFields(raw, &r.Error)
		if e == nil {
			e = r.Error.Validate()
		}
	default:
		return errors.New("unsupported response frame type")
	}
	for k := range m {
		if !allowed[k] {
			return errors.New("unexpected response frame field")
		}
	}
	return e
}

func decodeFixedFields(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func DecodeRequest(b []byte, logical contract.Limits) (Request, error) {
	var r Request
	e := r.decodeJSON(b, logical)
	return r, e
}
func (r *Request) UnmarshalJSON(b []byte) error { return r.decodeJSON(b) }
func (r *Request) decodeJSON(b []byte, logical ...contract.Limits) error {
	value, e := contract.DecodeWireJSON(b, 16<<20, logical...)
	if e != nil {
		return e
	}
	m, ok := value.(map[string]any)
	if !ok {
		return errors.New("request frame must be object")
	}
	for k := range m {
		switch k {
		case "type", "id", "capability", "params", "deadline", "caller_binding", "definition_hash":
		default:
			return errors.New("unexpected request frame field")
		}
	}
	type plain Request
	var decoded plain
	// Decode again only into the typed frame. Params retain the common decoder's
	// exact int64/finite-number semantics rather than json.Unmarshal float64.
	if e = json.Unmarshal(b, &decoded); e != nil {
		return e
	}
	decoded.Params, ok = m["params"].(map[string]any)
	if !ok {
		return errors.New("params must be object")
	}
	if _, ok := m["capability"].(string); !ok {
		return errors.New("capability must be string")
	}
	if decoded.Type != "request" || decoded.ID == "" || decoded.Deadline.IsZero() {
		return errors.New("incomplete request frame")
	}
	*r = Request(decoded)
	return nil
}

func (r Request) MarshalJSON() ([]byte, error) {
	type plain Request
	if r.Params == nil {
		r.Params = map[string]any{}
	}
	return json.Marshal(plain(r))
}
