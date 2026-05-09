package gateway

import (
	"crypto/rand"
	"time"

	"github.com/oklog/ulid/v2"
)

func newID() string {
	id, err := ulid.New(ulid.Timestamp(time.Now()), rand.Reader)
	if err != nil {
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	return id.String()
}
