package protocol

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestTransferStrictFramesAndChunkLimits(t *testing.T) {
	frames := []Frame{{Type: "request_begin", ID: "request", Capability: "run", CallerBinding: "caller", DefinitionHash: strings.Repeat("a", 64), InputDeadline: time.Now().Add(time.Minute)}, {Type: "input_open", ID: "request", TransferID: "transfer", Pointer: "/doc", Name: "a.bin", MIMEType: "application/octet-stream"}, {Type: "transfer_chunk", ID: "request", TransferID: "transfer", Sequence: 0, DataBase64: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{255}, ChunkBytes))}, {Type: "ack", TransferID: "transfer", Sequence: 0}, {Type: "request_commit", ID: "request", Params: map[string]any{"doc": nil, "n": int64(9007199254740993)}, Files: []FileBinding{{Pointer: "/doc", FileID: "file"}}}}
	for _, f := range frames {
		raw := MustJSON(f)
		decoded, e := DecodeFrame(raw)
		if e != nil || decoded.Type != f.Type {
			t.Fatal(f.Type, e)
		}
		if f.Type == "request_commit" && decoded.Params["n"] != int64(9007199254740993) {
			t.Fatal("integer precision lost")
		}
		if _, e = DecodeFrame(append(raw[:len(raw)-1], []byte(`,"unknown":0}`)...)); e == nil {
			t.Fatal("unknown field accepted")
		}
	}
	invalid := []string{`{"type":"ack","transfer_id":"x","sequence":0,"id":"request"}`, `{"type":"ack","transfer_id":"x","sequence":-1}`, `{"type":"ack","transfer_id":"x","sequence":0,"sequence":1}`, `{"type":"transfer_chunk","id":"r","transfer_id":"t","sequence":0,"data_base64":"Zh=="}`, `{"type":"request_commit","id":"r","params":{},"files":[{"pointer":"/f","file_id":"x","unknown":1}]}`}
	for _, raw := range invalid {
		if _, e := DecodeFrame([]byte(raw)); e == nil {
			t.Fatal("invalid accepted", raw)
		}
	}
	f := frames[2]
	f.DataBase64 = base64.StdEncoding.EncodeToString(make([]byte, ChunkBytes+1))
	if _, e := DecodeFrame(MustJSON(f)); e == nil {
		t.Fatal("oversized chunk accepted")
	}
}
