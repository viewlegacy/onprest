package agent

import "github.com/viewlegacy/onprest/internal/valueformat"

func validateFormat(format, value string) error { return valueformat.Validate(format, value) }
