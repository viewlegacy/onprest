package contract

import (
	"errors"
	"regexp"
	"unicode"
	"unicode/utf8"
)

// PublicFileDescriptor is transport metadata only; it never carries a reader,
// body, local path, or credential. URI and expiry are supplied by the later
// Gateway delivery layer. Inline descriptors omit both fields.
type PublicFileDescriptor struct {
	ID string `json:"id"`
	ArtifactMetadata
	URI       string `json:"uri,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}
type ResultValue struct {
	Data  any                    `json:"data"`
	Files []PublicFileDescriptor `json:"files"`
}
type SuccessEnvelope struct {
	RequestID string      `json:"request_id"`
	Result    ResultValue `json:"result"`
}

func NewSuccessEnvelope(requestID string, data any, files []PublicFileDescriptor) SuccessEnvelope {
	if files == nil {
		files = []PublicFileDescriptor{}
	}
	return SuccessEnvelope{requestID, ResultValue{data, files}}
}

type ErrorValue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Reason  string `json:"reason,omitempty"`
	Outcome string `json:"outcome"`
}
type ErrorEnvelope struct {
	RequestID string     `json:"request_id"`
	Error     ErrorValue `json:"error"`
}

var appCodeRE = regexp.MustCompile(`^APP_[A-Z][A-Z0-9_]{0,59}$`)
var reasonCodeRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Validate checks a public error value without turning a raw error into a
// public message or inferring business outcome from its code. APP_* messages
// are explicitly supplied public information; system messages remain owned by
// the controller. Byte limits are server constraints, not JSON maxLength.
func (e ErrorValue) Validate() error {
	if !contains(systemErrorCodes, e.Code) && !appCodeRE.MatchString(e.Code) {
		return errors.New("invalid public error code")
	}
	if !utf8.ValidString(e.Message) || len(e.Message) < 1 || len(e.Message) > 1024 {
		return errors.New("invalid public error message")
	}
	for _, r := range e.Message {
		if unicode.IsControl(r) {
			return errors.New("invalid public error message")
		}
	}
	if e.Reason != "" && !reasonCodeRE.MatchString(e.Reason) {
		return errors.New("invalid safe reason code")
	}
	if !contains(outcomes, e.Outcome) {
		return errors.New("invalid error outcome")
	}
	if appCodeRE.MatchString(e.Code) && e.Reason != "" {
		return errors.New("public application error cannot specify a reason")
	}
	return nil
}
