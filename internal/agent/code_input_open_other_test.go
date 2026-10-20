//go:build !linux && !darwin && !windows

package agent

import "testing"

func codeUnsafeNativeInputs(*testing.T, string) map[string]string { return map[string]string{} }
