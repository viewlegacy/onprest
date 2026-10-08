//go:build wasip1 && wasm

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
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
func badCall(method string, args any) int64 {
	b, _ := json.Marshal(map[string]any{"method": method, "args": args})
	return hostCall(ptr(b), uint32(len(b)))
}
func main() {
	var input struct {
		Scenario string `json:"scenario"`
		Size     int    `json:"size"`
		Envelope string `json:"envelope"`
	}
	if json.NewDecoder(os.Stdin).Decode(&input) != nil {
		panic("invalid input")
	}
	switch input.Scenario {
	case "missing":
		fmt.Print(`{"data":"forged","files":[]}`)
		return
	case "loop":
		fmt.Fprintln(os.Stderr, "worker-entered")
		for {
		}
	case "memory":
		b := make([]byte, 256<<20)
		for i := range b {
			b[i] = 1
		}
		fmt.Println(b[0])
		return
	case "large":
		prefix := `{"data":"`
		suffix := `","files":[]}`
		n := input.Size - len(prefix) - len(suffix)
		result([]byte(prefix + strings.Repeat("a", n) + suffix))
		return
	case "envelope":
		result([]byte(input.Envelope))
		return
	case "early":
		h := call("result.open", map[string]any{})
		badCall("result.commit", map[string]any{"result_handle": h["result_handle"]})
		return
	case "duplicate_open":
		call("result.open", map[string]any{})
		badCall("result.open", map[string]any{})
		return
	case "foreign":
		h := call("result.open", map[string]any{})
		stream(h["body_handle"], []byte(`{"data":null,"files":[]}`))
		badCall("result.commit", map[string]any{"result_handle": h["result_handle"] + 1})
		return
	case "bad_pointer":
		if hostCall(0xffffffff, 64) != -1 {
			panic("pointer boundary bypass")
		}
		result([]byte(`{"data":"bounded","files":[]}`))
		return
	case "chunk_pointer":
		buffer := make([]byte, 65537)
		if hostCall(ptr(buffer), 65537) != -4 || hostRead(1, ptr(buffer), 65537) != -4 || hostWrite(1, ptr(buffer), 65537) != -4 {
			panic("chunk limit bypass")
		}
		if hostRead(1, 0xffffffff, 64) != -1 || hostWrite(1, 0xffffffff, 64) != -1 {
			panic("read/write pointer overflow")
		}
		result([]byte(`{"data":"bounded","files":[]}`))
		return
	case "catch_limit":
		h := call("result.file", map[string]any{"name": "quota.bin", "mime_type": "application/octet-stream"})
		buffer := make([]byte, 4096)
		for i := 0; i < input.Size; i += len(buffer) {
			n := hostWrite(h["body_handle"], ptr(buffer), uint32(len(buffer)))
			if n == -4 {
				if hostClose(h["body_handle"]) != 0 || hostClose(h["file_handle"]) != 0 {
					panic("limited output cleanup")
				}
				result([]byte(`{"error":{"kind":"internal","category":"limit"}}`))
				return
			}
			if n != 4096 {
				panic("unexpected limited write")
			}
		}
		panic("expected host limit was not reached")
	case "catch_total":
		first := call("result.file", map[string]any{"name": "first.bin", "mime_type": "application/octet-stream"})
		stream(first["body_handle"], make([]byte, 8192))
		second := call("result.file", map[string]any{"name": "second.bin", "mime_type": "application/octet-stream"})
		buffer := make([]byte, 4096)
		if hostWrite(second["body_handle"], ptr(buffer), 4096) != -4 {
			panic("total file limit bypass")
		}
		if hostClose(second["body_handle"]) != 0 || hostClose(first["file_handle"]) != 0 || hostClose(second["file_handle"]) != 0 {
			panic("total limit cleanup")
		}
		result([]byte(`{"error":{"kind":"internal","category":"limit"}}`))
		return
	case "unreferenced":
		h := call("result.file", map[string]any{"name": "unused.bin", "mime_type": "application/octet-stream"})
		stream(h["body_handle"], []byte("not public"))
		result([]byte(`{"data":"only-data","files":[]}`))
		return
	case "two_files":
		call("result.file", map[string]any{"name": "one.bin", "mime_type": "application/octet-stream"})
		call("result.file", map[string]any{"name": "two.bin", "mime_type": "application/octet-stream"})
		return
	case "unready_file":
		h := call("result.file", map[string]any{"name": "unready.bin", "mime_type": "application/octet-stream"})
		result([]byte(fmt.Sprintf(`{"data":null,"files":[{"handle":%d}]}`, h["file_handle"])))
		return
	case "bad_handle":
		buf := make([]byte, 8)
		if hostRead(0x123456, ptr(buf), 8) != -1 || hostWrite(0x123456, ptr(buf), 8) != -1 || hostClose(0x123456) != -1 {
			panic("foreign handle accepted")
		}
		result([]byte(`{"data":"bounded","files":[]}`))
		return
	case "business":
		if badCall("input.open", map[string]any{"file_id": "foreign"}) != -2 || badCall("http.open", map[string]any{}) != -2 || badCall("files.open", map[string]any{}) != -7 {
			panic("business access granted")
		}
		result([]byte(`{"data":"denied","files":[]}`))
		return
	case "logs":
		for i := 0; i < 80; i++ {
			fmt.Print(strings.Repeat("o", 16384))
			fmt.Fprint(os.Stderr, strings.Repeat("e", 16384))
			call("log.write", map[string]any{"level": "debug", "message": strings.Repeat("l", 16384)})
		}
		fmt.Fprint(os.Stderr, "TAIL-MARKER\x00\xff\n")
		result([]byte(`{"data":"logged","files":[]}`))
		return
	case "panic_logs":
		for i := 0; i < 80; i++ {
			fmt.Fprint(os.Stderr, strings.Repeat("e", 16384))
		}
		fmt.Fprint(os.Stderr, "TAIL-MARKER\x00\xff\n")
		panic("PRIVATE-PANIC-DETAIL")
	case "utf8":
		fmt.Print(strings.Repeat("界", 7000) + "\xff\x00\n")
		fmt.Fprint(os.Stderr, "warn\n")
		call("log.write", map[string]any{"level": "error", "message": "error level only"})
		call("log.write", map[string]any{"level": "WRONG", "message": "DISCARD"})
		result([]byte(`{"data":"logged","files":[]}`))
		return
	}
	file := call("result.file", map[string]any{"name": "artifact.bin", "mime_type": "application/octet-stream"})
	stream(file["body_handle"], []byte(strings.Repeat("F", 70000)))
	body, _ := json.Marshal(map[string]any{"data": map[string]any{"integer": json.Number("9007199254740993")}, "files": []any{map[string]any{"handle": file["file_handle"]}}})
	if input.Scenario == "duplicate_file" {
		body, _ = json.Marshal(map[string]any{"data": nil, "files": []any{map[string]any{"handle": file["file_handle"]}, map[string]any{"handle": file["file_handle"]}}})
	}
	id := result(body)
	switch input.Scenario {
	case "post_close":
		if hostClose(file["file_handle"]) != 0 || hostClose(id) != 0 {
			panic("committed handle close rejected")
		}
	case "duplicate_commit":
		badCall("result.commit", map[string]any{"result_handle": id})
	case "post_io":
		badCall("result.file", map[string]any{"name": "late", "mime_type": "text/plain"})
	case "post_crash":
		panic("PRIVATE-POST-COMMIT-PANIC")
	case "post_timeout":
		for {
		}
	}
}
