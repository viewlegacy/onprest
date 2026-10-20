package wasmruntime

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

const workerCommand = "--onprest-internal-wasm-worker"

type packet struct {
	Op       string    `json:"op"`
	Token    string    `json:"token,omitempty"`
	Handle   int64     `json:"handle,omitempty"`
	Data     []byte    `json:"data,omitempty"`
	Offset   int64     `json:"offset,omitempty"`
	Count    int32     `json:"count,omitempty"`
	Metadata *Metadata `json:"metadata,omitempty"`
	Error    string    `json:"error,omitempty"`
}
type bootstrap struct {
	Token       string            `json:"token"`
	Limits      Limits            `json:"limits"`
	Deadline    int64             `json:"deadline"`
	ModuleSize  int               `json:"module_size"`
	ParamsSize  int               `json:"params_size"`
	Validate    bool              `json:"validate"`
	Version     string            `json:"version"`
	Environment map[string]string `json:"environment,omitempty"`
	Base        string            `json:"base,omitempty"`
	Business    bool              `json:"business,omitempty"`
}

func send(w io.Writer, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	if len(b) > 2*Chunk {
		return errors.New("worker packet exceeds limit")
	}
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(b)))
	if e = writeAll(w, h[:]); e != nil {
		return e
	}
	return writeAll(w, b)
}
func receive(r io.Reader, v any) error {
	var h [4]byte
	if _, e := io.ReadFull(r, h[:]); e != nil {
		return e
	}
	n := binary.BigEndian.Uint32(h[:])
	if n == 0 || n > 2*Chunk {
		return errors.New("invalid worker packet size")
	}
	b := make([]byte, n)
	if _, e := io.ReadFull(r, b); e != nil {
		return e
	}
	return json.Unmarshal(b, v)
}
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, e := w.Write(b)
		if e != nil {
			return e
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
