//go:build !darwin && !linux

package launchagent

import "errors"

func platformExchange(int, string, string) error {
	return errors.New("atomic entry exchange is unsupported")
}

func platformRenameExclusive(int, string, string) error {
	return errors.New("exclusive atomic rename is unsupported")
}
