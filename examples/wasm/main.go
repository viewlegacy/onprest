//go:build wasip1 && wasm

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"unsafe"
)

//go:wasmimport onprest_host_v1 call
func hostCall(ptr uint32, n uint32) int64

//go:wasmimport onprest_host_v1 read
func hostRead(h int64, ptr uint32, n uint32) int32

//go:wasmimport onprest_host_v1 write
func hostWrite(h int64, ptr uint32, n uint32) int32

//go:wasmimport onprest_host_v1 close
func hostClose(h int64) int32
func ptr(b []byte) uint32 {
	if len(b) == 0 {
		return 0
	}
	return uint32(uintptr(unsafe.Pointer(&b[0])))
}
func call(method string, args any) map[string]int64 {
	b, _ := json.Marshal(map[string]any{"method": method, "args": args})
	h := hostCall(ptr(b), uint32(len(b)))
	if h < 1 {
		panic(fmt.Sprintf("host call rejected: %d", h))
	}
	var data []byte
	for {
		buf := make([]byte, 4096)
		n := hostRead(h, ptr(buf), uint32(len(buf)))
		if n < 0 {
			panic("control read failed")
		}
		data = append(data, buf[:n]...)
		if n == 0 {
			break
		}
	}
	if hostClose(h) != 0 {
		panic("control close failed")
	}
	var out map[string]int64
	if json.Unmarshal(data, &out) != nil {
		panic("control decode")
	}
	return out
}
func stream(h int64, b []byte) {
	for len(b) > 0 {
		n := len(b)
		if n > 65536 {
			n = 65536
		}
		w := hostWrite(h, ptr(b), uint32(n))
		if w < 0 {
			panic(fmt.Sprintf("host write rejected: %d", w))
		}
		if w == 0 {
			panic("zero write")
		}
		b = b[w:]
	}
	if hostClose(h) != 0 {
		panic("body close failed")
	}
}
func result(b []byte) int64 {
	h := call("result.open", map[string]any{})
	stream(h["body_handle"], b)
	call("result.commit", map[string]any{"result_handle": h["result_handle"]})
	return h["result_handle"]
}
func main() {
	var input struct {
		Name       string `json:"name"`
		Attachment struct {
			File struct {
				ID string `json:"file_id"`
			} `json:"__onprest_input_file_v1"`
		} `json:"attachment"`
	}
	if json.NewDecoder(os.Stdin).Decode(&input) != nil {
		panic("invalid input")
	}
	greeting, e := os.ReadFile("message.txt")
	if e != nil {
		panic(e)
	}
	h := call("input.open", map[string]any{"file_id": input.Attachment.File.ID})["body_handle"]
	var body []byte
	for {
		buf := make([]byte, 4096)
		n := hostRead(h, ptr(buf), uint32(len(buf)))
		if n < 0 {
			panic("input read")
		}
		if n == 0 {
			break
		}
		body = append(body, buf[:n]...)
	}
	if hostClose(h) != 0 {
		panic("input close")
	}
	file := call("result.file", map[string]any{"name": "copy.bin", "mime_type": "application/octet-stream"})
	stream(file["body_handle"], body)
	envelope, _ := json.Marshal(map[string]any{"data": map[string]any{"greeting": string(greeting) + input.Name, "input_bytes": len(body), "private": "removed by output"}, "files": []any{map[string]any{"handle": file["file_handle"]}}})
	result(envelope)
}
