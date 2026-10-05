package protocol

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/viewlegacy/onprest/internal/contract"
	"strings"
	"time"
	"unicode/utf8"
)

const ChunkBytes = 64 << 10
const TransferWindow = 4
const ConnectionWindow = 32
const ManagementBytes = 1 << 20

type FileBinding struct {
	Pointer string `json:"pointer"`
	FileID  string `json:"file_id"`
}

// Frame is the fixed, internal input/transfer protocol. ACK deliberately has no
// request ID: its transfer ID is resolved against the authenticated connection.
type Frame struct {
	Type           string         `json:"type"`
	ID             string         `json:"id,omitempty"`
	TransferID     string         `json:"transfer_id,omitempty"`
	Capability     string         `json:"capability,omitempty"`
	CallerBinding  string         `json:"caller_binding,omitempty"`
	DefinitionHash string         `json:"definition_hash,omitempty"`
	InputDeadline  time.Time      `json:"input_deadline,omitempty"`
	Pointer        string         `json:"pointer,omitempty"`
	Name           string         `json:"name,omitempty"`
	MIMEType       string         `json:"mime_type,omitempty"`
	FileID         string         `json:"file_id,omitempty"`
	Sequence       int64          `json:"sequence,omitempty"`
	DataBase64     string         `json:"data_base64,omitempty"`
	Size           int64          `json:"size,omitempty"`
	SizeBytes      int64          `json:"size_bytes,omitempty"`
	SHA256         string         `json:"sha256,omitempty"`
	Params         map[string]any `json:"params,omitempty"`
	Files          []FileBinding  `json:"files,omitempty"`
}

var fields = map[string]string{
	"request_begin": "id capability caller_binding definition_hash input_deadline", "request_accept": "id",
	"input_open": "id transfer_id pointer name mime_type", "input_accept": "id transfer_id",
	"input_ready": "id transfer_id file_id size_bytes sha256", "request_commit": "id params files",
	"transfer_open": "id transfer_id file_id", "transfer_chunk": "id transfer_id sequence data_base64",
	"transfer_end": "id transfer_id size sha256", "transfer_cancel": "id transfer_id", "ack": "transfer_id sequence",
}

func IsTransferType(kind string) bool { _, ok := fields[kind]; return ok }
func ValidID(id string) bool {
	return id != "" && len(id) <= 256 && utf8.ValidString(id) && !strings.ContainsRune(id, 0)
}
func (f Frame) MarshalJSON() ([]byte, error) {
	names, ok := fields[f.Type]
	if !ok {
		return nil, errors.New("unsupported transfer frame")
	}
	m := map[string]any{"type": f.Type}
	for _, key := range strings.Fields(names) {
		switch key {
		case "id":
			m[key] = f.ID
		case "transfer_id":
			m[key] = f.TransferID
		case "capability":
			m[key] = f.Capability
		case "caller_binding":
			m[key] = f.CallerBinding
		case "definition_hash":
			m[key] = f.DefinitionHash
		case "input_deadline":
			m[key] = f.InputDeadline
		case "pointer":
			m[key] = f.Pointer
		case "name":
			m[key] = f.Name
		case "mime_type":
			m[key] = f.MIMEType
		case "file_id":
			m[key] = f.FileID
		case "sequence":
			m[key] = f.Sequence
		case "data_base64":
			m[key] = f.DataBase64
		case "size":
			m[key] = f.Size
		case "size_bytes":
			m[key] = f.SizeBytes
		case "sha256":
			m[key] = f.SHA256
		case "params":
			m[key] = f.Params
		case "files":
			m[key] = f.Files
		}
	}
	return json.Marshal(m)
}
func DecodeFrame(raw []byte) (Frame, error) {
	var f Frame
	value, e := contract.DecodeJSON(raw, contract.Limits{MaxBytes: ManagementBytes, MaxDepth: 32, MaxItems: 100000})
	if e != nil {
		return f, e
	}
	m, ok := value.(map[string]any)
	if !ok {
		return f, errors.New("invalid transfer frame")
	}
	kind, _ := m["type"].(string)
	names, ok := fields[kind]
	if !ok {
		return f, errors.New("unsupported transfer frame")
	}
	allowed := map[string]bool{"type": true}
	for _, k := range strings.Fields(names) {
		allowed[k] = true
		if _, ok := m[k]; !ok {
			return f, errors.New("incomplete transfer frame")
		}
	}
	for k := range m {
		if !allowed[k] {
			return f, errors.New("unexpected transfer field")
		}
	}
	type plain Frame
	var p plain
	if e = json.Unmarshal(raw, &p); e != nil {
		return f, e
	}
	f = Frame(p)
	if kind != "ack" && !ValidID(f.ID) || strings.Contains(names, "transfer_id") && !ValidID(f.TransferID) {
		return f, errors.New("invalid transfer identity")
	}
	if f.Sequence < 0 || f.Size < 0 || f.SizeBytes < 0 {
		return f, errors.New("invalid transfer count")
	}
	if kind == "request_commit" {
		params, ok := m["params"].(map[string]any)
		if !ok || f.Files == nil {
			return f, errors.New("invalid commit")
		}
		f.Params = params
		for _, item := range m["files"].([]any) {
			binding, ok := item.(map[string]any)
			if !ok || len(binding) != 2 || binding["pointer"] == nil || binding["file_id"] == nil {
				return f, errors.New("invalid file binding fields")
			}
		}
		for _, v := range f.Files {
			if !ValidID(v.FileID) || len(v.Pointer) > 4096 {
				return f, errors.New("invalid file binding")
			}
		}
	}
	if kind == "request_begin" && (!ValidID(f.CallerBinding) || !ValidID(f.Capability) || !ValidID(f.DefinitionHash) || f.InputDeadline.IsZero()) {
		return f, errors.New("invalid begin")
	}
	if kind == "input_open" && (len(f.Pointer) > 4096 || len(f.Name) > 255 || len(f.MIMEType) > 255) {
		return f, errors.New("invalid input metadata")
	}
	if kind == "transfer_chunk" {
		if _, e = f.Chunk(); e != nil {
			return f, e
		}
	}
	return f, nil
}
func (f Frame) Chunk() ([]byte, error) {
	if len(f.DataBase64) > base64.StdEncoding.EncodedLen(ChunkBytes) {
		return nil, errors.New("chunk too large")
	}
	b, e := base64.StdEncoding.Strict().DecodeString(f.DataBase64)
	if e != nil || len(b) == 0 || len(b) > ChunkBytes || base64.StdEncoding.EncodeToString(b) != f.DataBase64 {
		return nil, errors.New("invalid chunk")
	}
	return b, nil
}
