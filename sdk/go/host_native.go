//go:build !wasip1 || !wasm

package onprest

type nativeHost struct{}

func (nativeHost) call([]byte) int64         { return -7 }
func (nativeHost) read(int64, []byte) int32  { return -7 }
func (nativeHost) write(int64, []byte) int32 { return -7 }
func (nativeHost) close(int64) int32         { return -7 }
func platformHost() host                     { return nativeHost{} }
