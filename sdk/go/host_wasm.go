//go:build wasip1 && wasm

package onprest

import "unsafe"

//go:wasmimport onprest_host_v1 call
func importCall(uint32, uint32) int64

//go:wasmimport onprest_host_v1 read
func importRead(int64, uint32, uint32) int32

//go:wasmimport onprest_host_v1 write
func importWrite(int64, uint32, uint32) int32

//go:wasmimport onprest_host_v1 close
func importClose(int64) int32
func pointer(b []byte) uint32 {
	if len(b) == 0 {
		return 0
	}
	return uint32(uintptr(unsafe.Pointer(&b[0])))
}

type wasmHost struct{}

func (wasmHost) call(b []byte) int64           { return importCall(pointer(b), uint32(len(b))) }
func (wasmHost) read(h int64, b []byte) int32  { return importRead(h, pointer(b), uint32(len(b))) }
func (wasmHost) write(h int64, b []byte) int32 { return importWrite(h, pointer(b), uint32(len(b))) }
func (wasmHost) close(h int64) int32           { return importClose(h) }
func platformHost() host                       { return wasmHost{} }
