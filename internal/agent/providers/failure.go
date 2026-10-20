package providers

import "github.com/viewlegacy/onprest/internal/contract"

// Failure supplies only an internal system category and fixed safe reason. The
// controller supplies the public message, validates the category/reason, and
// decides outcome from Effects. Cause belongs exclusively to local diagnostics.
type Failure struct {
	Code, Reason string
	Cause        error
}

func (f *Failure) Error() string {
	if f.Cause != nil {
		return f.Cause.Error()
	}
	return "executor failed"
}

// PublicTerminal carries an explicitly public raw result ABI error. General
// provider errors cannot acquire this trust by using an APP_* category alone.
// The controller revalidates Code/Message and supplies the observed outcome.
type PublicTerminal struct {
	Code, Message string
}

func NewPublicTerminal(code, message string) (*PublicTerminal, error) {
	v := contract.ErrorValue{Code: code, Message: message, Outcome: "unknown"}
	if len(code) < 4 || code[:4] != "APP_" {
		return nil, &Failure{Code: "EXECUTION_FAILED"}
	}
	if err := v.Validate(); err != nil {
		return nil, &Failure{Code: "EXECUTION_FAILED", Cause: err}
	}
	return &PublicTerminal{Code: code, Message: message}, nil
}

func (e *PublicTerminal) Error() string { return "public application terminal: " + e.Code }
