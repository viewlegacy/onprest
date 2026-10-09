package contract

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"strings"
)

// DecodeInlineFile checks the public wire representation before returning an
// incremental reader. No host path or request identity is accepted from wire.
func DecodeInlineFile(raw any, limit int64) (ArtifactMetadata, io.Reader, error) {
	var m ArtifactMetadata
	object, ok := raw.(map[string]any)
	if !ok || len(object) != 3 {
		return m, nil, errors.New("invalid inline file")
	}
	name, nok := object["name"].(string)
	media, mok := object["mime_type"].(string)
	data, dok := object["data"].(string)
	if !nok || !mok || !dok || ValidateBasename(name) != nil {
		return m, nil, errors.New("invalid inline file")
	}
	mt, _, e := mime.ParseMediaType(media)
	mt = strings.ToLower(mt)
	if e != nil || !validMIME(mt) {
		return m, nil, errors.New("invalid inline MIME")
	}
	if len(data)%4 != 0 || strings.IndexFunc(data, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '/' || r == '=')
	}) >= 0 {
		return m, nil, errors.New("invalid inline Base64")
	}
	hash := sha256.New()
	reader := base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(data))
	var b [64 << 10]byte
	var size int64
	for {
		n, e := reader.Read(b[:])
		size += int64(n)
		hash.Write(b[:n])
		if size > limit {
			return m, nil, &OutputError{Code: "LIMIT_EXCEEDED"}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return m, nil, errors.New("invalid inline Base64")
		}
	}
	m = ArtifactMetadata{Name: name, MIMEType: mt, SizeBytes: size, SHA256: hex.EncodeToString(hash.Sum(nil))}
	return m, base64.NewDecoder(base64.StdEncoding.Strict(), strings.NewReader(data)), nil
}
