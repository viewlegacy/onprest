package providers

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
