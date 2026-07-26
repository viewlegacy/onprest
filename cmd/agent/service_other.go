//go:build !windows

package main

func runAsPlatformService([]string) (bool, error) {
	return false, nil
}
