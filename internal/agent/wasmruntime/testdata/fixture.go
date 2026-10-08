//go:build wasip1 && wasm

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		panic("control decode")
	}
	out := map[string]int64{}
	for name, raw := range fields {
		var value int64
		if json.Unmarshal(raw, &value) == nil {
			out[name] = value
		}
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

//go:wasmimport wasi_snapshot_preview1 fd_allocate
func fdAllocate(fd uint32, offset, length uint64) uint32

//go:wasmimport wasi_snapshot_preview1 fd_advise
func fdAdvise(fd uint32, offset, length uint64, advice uint32) uint32

//go:wasmimport wasi_snapshot_preview1 fd_seek
func fdSeek(fd uint32, offset int64, whence uint32, result uint32) uint32

//go:wasmimport wasi_snapshot_preview1 fd_readdir
func fdReaddir(fd, buf, length uint32, cookie uint64, used uint32) uint32

//go:wasmimport wasi_snapshot_preview1 fd_filestat_get
func fdStat(fd, buf uint32) uint32

//go:wasmimport wasi_snapshot_preview1 fd_sync
func fdSync(fd uint32) uint32

//go:wasmimport wasi_snapshot_preview1 fd_datasync
func fdDatasync(fd uint32) uint32

//go:wasmimport wasi_snapshot_preview1 path_unlink_file
func pathUnlink(fd, path, length uint32) uint32

//go:wasmimport wasi_snapshot_preview1 path_rename
func pathRename(oldfd, oldpath, oldlen, newfd, newpath, newlen uint32) uint32

//go:wasmimport wasi_snapshot_preview1 path_create_directory
func pathMkdir(fd, path, length uint32) uint32
func main() {
	var input struct {
		Scenario string `json:"scenario"`
		Size     int    `json:"size"`
		Envelope string `json:"envelope"`
		Path     string `json:"path"`
		URL      string `json:"url"`
		FileID   string `json:"file_id"`
		Steps    []struct {
			Method string         `json:"method"`
			Args   map[string]any `json:"args"`
			Data   string         `json:"data"`
			Read   bool           `json:"read"`
			Raw    string         `json:"raw"`
		} `json:"steps"`
	}
	if json.NewDecoder(os.Stdin).Decode(&input) != nil {
		panic("invalid input")
	}
	switch input.Scenario {
	case "review2_dir":
		f, e := os.Open(input.Path)
		if e != nil {
			panic(e)
		}
		if _, e = f.Stat(); e != nil {
			panic(e)
		}
		hostClose(call("input.open", map[string]any{"file_id": "swap"})["body_handle"])
		entries, e := f.ReadDir(-1)
		if e != nil {
			panic(e)
		}
		first := []string{}
		for _, v := range entries {
			first = append(first, v.Name())
		}
		hostClose(call("input.open", map[string]any{"file_id": "rewind"})["body_handle"])
		if _, e = f.Seek(0, io.SeekStart); e != nil {
			panic(e)
		}
		entries, e = f.ReadDir(-1)
		if e != nil {
			panic(e)
		}
		last := []string{}
		for _, v := range entries {
			last = append(last, v.Name())
		}
		f.Close()
		b, _ := json.Marshal(map[string]any{"data": map[string]any{"first": first, "last": last}, "files": []any{}})
		result(b)
		return
	case "review2_sync":
		f, e := os.Open(input.Path)
		if e != nil {
			panic(e)
		}
		fd := uint32(f.Fd())
		sync, datasync := fdSync(fd), fdDatasync(fd)
		f.Close()
		b, _ := json.Marshal(map[string]any{"data": map[string]any{"sync": sync, "datasync": datasync, "closed_sync": fdSync(fd), "closed_datasync": fdDatasync(fd)}, "files": []any{}})
		result(b)
		return
	case "review2_positioned":
		f, e := os.OpenFile(input.Path, os.O_RDWR, 0)
		if e != nil {
			panic(e)
		}
		before, _ := f.Seek(1, io.SeekStart)
		buf := make([]byte, 2)
		_, e = f.ReadAt(buf, 1)
		if e != nil {
			panic(e)
		}
		n, e := f.WriteAt([]byte("XY"), 1)
		if e != nil {
			panic(e)
		}
		after, _ := f.Seek(0, io.SeekCurrent)
		f.Close()
		f, e = os.Open(input.Path)
		if e != nil {
			panic(e)
		}
		fd := uint32(f.Fd())
		sync, datasync := fdSync(fd), fdDatasync(fd)
		f.Close()
		closed := fdSync(fd)
		b, _ := json.Marshal(map[string]any{"data": map[string]any{"read": string(buf), "written": n, "before": before, "after": after, "sync": sync, "datasync": datasync, "closed_sync": closed}, "files": []any{}})
		result(b)
		return
	case "review_rawdir":
		f, e := os.Open(input.Path)
		if e != nil {
			panic(e)
		}
		entries, e := f.ReadDir(-1)
		f.Close()
		names := []string{}
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		code := ""
		if e != nil {
			code = e.Error()
		}
		b, _ := json.Marshal(map[string]any{"data": map[string]any{"names": names, "error": code}, "files": []any{}})
		result(b)
		return
	case "review_raweof":
		f, e := os.Open(input.Path)
		if e != nil {
			panic(e)
		}
		b := make([]byte, 1)
		first, _ := f.Read(b)
		w, e := os.OpenFile(input.Path, os.O_RDWR, 0)
		if e != nil {
			panic(e)
		}
		if e = w.Truncate(int64(input.Size)); e != nil {
			panic(e)
		}
		w.Close()
		next, e := f.Read(b)
		f.Close()
		raw, _ := json.Marshal(map[string]any{"data": map[string]any{"first": first, "next": next, "error": e != nil && e != io.EOF}, "files": []any{}})
		result(raw)
		return
	case "review_eof":
		h := call("files.open", map[string]any{"path": input.Path})["body_handle"]
		b := make([]byte, 1)
		first := hostRead(h, ptr(b), 1)
		w := call("files.wasi_open", map[string]any{"path": input.Path, "flags": os.O_RDWR})["body_handle"]
		call("files.truncate", map[string]any{"body_handle": w, "size": input.Size})
		hostClose(w)
		next := hostRead(h, ptr(b), 1)
		hostClose(h)
		raw, _ := json.Marshal(map[string]any{"data": map[string]any{"first": first, "next": next}, "files": []any{}})
		result(raw)
		return
	case "review_seek":
		h := call("files.open", map[string]any{"path": input.Path})["body_handle"]
		codes := []int64{}
		for _, v := range []struct{ o, w int64 }{{-1, 0}, {0, 3}, {9223372036854775807, 2}, {2, 0}, {-1, 1}, {-1, 2}} {
			c := badCall("files.seek", map[string]any{"body_handle": h, "offset": v.o, "whence": v.w})
			if c > 0 {
				hostClose(c)
				c = 0
			}
			codes = append(codes, c)
		}
		hostClose(h)
		f, e := os.Open(input.Path)
		if e != nil {
			panic(e)
		}
		var pos uint64
		rawCodes := []uint32{fdSeek(uint32(f.Fd()), -1, 0, uint32(uintptr(unsafe.Pointer(&pos)))), fdSeek(uint32(f.Fd()), 0, 3, uint32(uintptr(unsafe.Pointer(&pos)))), fdSeek(uint32(f.Fd()), -1, 2, uint32(uintptr(unsafe.Pointer(&pos))))}
		f.Close()
		raw, _ := json.Marshal(map[string]any{"data": map[string]any{"abi": codes, "raw": rawCodes}, "files": []any{}})
		result(raw)
		return
	case "review_rawclose":
		f, e := os.Open(input.Path)
		if e != nil {
			panic(e)
		}
		fd := uint32(f.Fd())
		f.Close()
		buf := make([]byte, 128)
		var used uint32
		codes := []uint32{fdReaddir(fd, ptr(buf), 128, 0, uint32(uintptr(unsafe.Pointer(&used)))), fdStat(fd, ptr(buf))}
		raw, _ := json.Marshal(map[string]any{"data": codes, "files": []any{}})
		result(raw)
		return
	case "review_commit":
		h := call("result.open", map[string]any{})
		stream(h["body_handle"], []byte(`{"data":"ack-required","files":[]}`))
		for i := 0; i < input.Size; i++ {
			if badCall("log.write", map[string]any{"level": "debug", "message": "hold"}) < 1 {
				panic("log capacity")
			}
		}
		badCall("result.commit", map[string]any{"result_handle": h["result_handle"]})
		return
	case "review_httpabort":
		h := call("http.open", map[string]any{"method": "POST", "url": input.URL, "headers": map[string]any{}, "content_length": input.Size})
		if input.Envelope == "response" {
			hostClose(h["body_handle"])
			h["response_handle"] = call("http.do", map[string]any{"request_handle": h["request_handle"]})["body_handle"]
		}
		if input.Envelope == "readerfail" {
			hostClose(h["body_handle"])
		}
		call("http.abort", map[string]any{"request_handle": h["request_handle"]})
		b := []byte("x")
		codes := []int64{int64(hostWrite(h["body_handle"], ptr(b), 1)), int64(hostClose(h["body_handle"])), badCall("http.abort", map[string]any{"request_handle": h["request_handle"]})}
		if h["response_handle"] > 0 {
			codes = append(codes, int64(hostRead(h["response_handle"], ptr(b), 1)))
		}
		raw, _ := json.Marshal(map[string]any{"data": codes, "files": []any{}})
		result(raw)
		return
	case "review4_raw":
		values := []any{}
		for _, step := range input.Steps {
			code := uint32(0)
			switch step.Method {
			case "files.remove":
				p := []byte(strings.TrimPrefix(step.Args["path"].(string), "/"))
				code = pathUnlink(3, ptr(p), uint32(len(p)))
			case "files.rename":
				a := []byte(strings.TrimPrefix(step.Args["old_path"].(string), "/"))
				b := []byte(strings.TrimPrefix(step.Args["new_path"].(string), "/"))
				code = pathRename(3, ptr(a), uint32(len(a)), 3, ptr(b), uint32(len(b)))
			case "files.mkdir":
				p := []byte(strings.TrimPrefix(step.Args["path"].(string), "/"))
				code = pathMkdir(3, ptr(p), uint32(len(p)))
			case "files.mkdir_all":
				if e := os.MkdirAll(step.Args["path"].(string), 0700); e != nil {
					code = 1
				}
			}
			values = append(values, map[string]any{"code": code})
		}
		b, _ := json.Marshal(map[string]any{"data": values, "files": []any{}})
		result(b)
		return
	case "bridge":
		results := []any{}
		var request int64
		for _, step := range input.Steps {
			if step.Args == nil {
				step.Args = map[string]any{}
			}
			if step.Method == "http.do" || step.Method == "http.abort" {
				step.Args["request_handle"] = request
			}
			raw, _ := json.Marshal(map[string]any{"method": step.Method, "args": step.Args})
			if step.Raw != "" {
				raw = []byte(step.Raw)
			}
			h := hostCall(ptr(raw), uint32(len(raw)))
			entry := map[string]any{"code": h}
			var control map[string]any
			if h > 0 {
				data := []byte{}
				for {
					buf := make([]byte, 4096)
					n := hostRead(h, ptr(buf), uint32(len(buf)))
					if n < 0 {
						entry["read_code"] = n
						break
					}
					if n == 0 {
						break
					}
					data = append(data, buf[:n]...)
				}
				hostClose(h)
				decoder := json.NewDecoder(bytes.NewReader(data))
				decoder.UseNumber()
				decoder.Decode(&control)
				entry["control"] = control
				if v, ok := control["request_handle"].(json.Number); ok {
					request, _ = v.Int64()
				}
				if v, ok := control["body_handle"].(json.Number); ok {
					body, _ := v.Int64()
					if step.Read {
						data = nil
						for {
							buf := make([]byte, 65536)
							n := hostRead(body, ptr(buf), uint32(len(buf)))
							if n < 0 {
								entry["body_code"] = n
								break
							}
							if n == 0 {
								break
							}
							data = append(data, buf[:n]...)
						}
						entry["data"] = base64.StdEncoding.EncodeToString(data)
					} else {
						data, _ := base64.StdEncoding.DecodeString(step.Data)
						var codes []int32
						for len(data) > 0 {
							n := min(len(data), 65536)
							w := hostWrite(body, ptr(data), uint32(n))
							codes = append(codes, w)
							if w <= 0 {
								break
							}
							data = data[w:]
						}
						entry["write_codes"] = codes
					}
					entry["close_code"] = hostClose(body)
				}
			}
			results = append(results, entry)
		}
		data, _ := json.Marshal(map[string]any{"data": results, "files": []any{}})
		result(data)
		return
	case "rawinvalid":
		input.Path = string([]byte{255})
		fallthrough
	case "rawfs":
		code := ""
		body, e := os.ReadFile(input.Path)
		if e != nil {
			code = e.Error()
		}
		data, _ := json.Marshal(map[string]any{"data": map[string]any{"body": base64.StdEncoding.EncodeToString(body), "error": code, "pwd": os.Getenv("PWD"), "secret": os.Getenv("P15_SECRET")}, "files": []any{}})
		result(data)
		return
	case "knownhole":
		h := call("files.write_file", map[string]any{"path": input.Path, "size": int64(4)})["body_handle"]
		seek := badCall("files.seek", map[string]any{"body_handle": h, "offset": int64(100), "whence": int64(0)})
		if seek > 0 {
			hostClose(seek)
		}
		b := []byte("abcd")
		w := hostWrite(h, ptr(b), 4)
		closed := hostClose(h)
		data, _ := json.Marshal(map[string]any{"data": map[string]any{"write": w, "close": closed}, "files": []any{}})
		result(data)
		return
	case "rawallocate":
		f, e := os.OpenFile(input.Path, os.O_RDWR, 0)
		if e != nil {
			panic(e)
		}
		allocation := fdAllocate(uint32(f.Fd()), 0, uint64(input.Size))
		advice := fdAdvise(uint32(f.Fd()), 0, 1, 0)
		f.Close()
		data, _ := json.Marshal(map[string]any{"data": map[string]any{"allocation": allocation, "advice": advice}, "files": []any{}})
		result(data)
		return
	case "aftercommitfs":
		f, e := os.Open(input.Path)
		if e != nil {
			panic(e)
		}
		result([]byte(`{"data":null,"files":[]}`))
		_, e = f.Read(make([]byte, 1))
		if e == nil {
			panic("business I/O after commit")
		}
		f.Close()
		return
	case "rawlife":
		if e := os.MkdirAll(filepath.Join(input.Path, "a", "b"), 0755); e != nil {
			panic(e)
		}
		name := filepath.Join(input.Path, "a", "b", "x.bin")
		f, e := os.Create(name)
		if e != nil {
			panic(e)
		}
		if _, e = f.Write([]byte("abcd")); e != nil {
			panic(e)
		}
		if _, e = f.Seek(0, 0); e != nil {
			panic(e)
		}
		if _, e = f.Write([]byte("z")); e != nil {
			panic(e)
		}
		if e = f.Truncate(6); e != nil {
			panic(e)
		}
		if e = f.Close(); e != nil {
			panic(e)
		}
		body, e := os.ReadFile(name)
		if e != nil {
			panic(e)
		}
		entries, e := os.ReadDir(filepath.Dir(name))
		if e != nil || len(entries) != 1 {
			panic(fmt.Sprintf("readdir len=%d error=%v", len(entries), e))
		}
		if _, e = entries[0].Info(); e != nil {
			panic(e)
		}
		dst := name + ".moved"
		if e = os.Rename(name, dst); e != nil {
			panic(e)
		}
		if e = os.Remove(dst); e != nil {
			panic(e)
		}
		data, _ := json.Marshal(map[string]any{"data": base64.StdEncoding.EncodeToString(body), "files": []any{}})
		result(data)
		return
	case "rawseek":
		f, e := os.Open(input.Path)
		if e != nil {
			panic(e)
		}
		var codes []string
		for i := 0; i < 3; i++ {
			f.Seek(0, io.SeekStart)
			b := make([]byte, 4)
			_, e = io.ReadFull(f, b)
			if e != nil {
				codes = append(codes, e.Error())
			} else {
				codes = append(codes, string(b))
			}
		}
		f.Close()
		data, _ := json.Marshal(map[string]any{"data": codes, "files": []any{}})
		result(data)
		return

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
	case "envelope_create_existing", "envelope_create_new", "envelope_create_truncate", "envelope_create_write", "envelope_create_failed":
		flags := os.O_WRONLY | os.O_CREATE
		if input.Scenario == "envelope_create_truncate" {
			flags |= os.O_TRUNC
		}
		if input.Scenario == "envelope_create_failed" {
			flags |= os.O_EXCL
		}
		f, e := os.OpenFile(input.Path, flags, 0600)
		if input.Scenario == "envelope_create_failed" {
			if !os.IsExist(e) {
				panic("expected collision")
			}
		} else {
			if e != nil {
				panic(e)
			}
			if input.Scenario == "envelope_create_write" {
				if _, e = f.Write([]byte("X")); e != nil {
					panic(e)
				}
			}
			if e = f.Close(); e != nil {
				panic(e)
			}
		}
		result([]byte(input.Envelope))
		return
	case "envelope_effect":
		if e := os.WriteFile(input.Path, []byte("observed"), 0600); e != nil {
			panic(e)
		}
		result([]byte(input.Envelope))
		return
	case "privatefailure":
		panic("PRIVATE_DRIVER_SECRET")
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
