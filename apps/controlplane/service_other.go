//go:build !windows

package main

import "errors"

func serviceInstall() error {
	return errors.New("service management is only supported on Windows")
}

func serviceUninstall() error {
	return errors.New("service management is only supported on Windows")
}

func serviceRun(_ []string) error {
	return errors.New("service management is only supported on Windows")
}
