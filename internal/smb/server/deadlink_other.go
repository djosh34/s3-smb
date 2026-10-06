//go:build !linux && !darwin

package server

import (
	"errors"
	"time"
)

// setUserTimeout fails where s3-smb has no way to bound unacknowledged data.
func setUserTimeout(uintptr, time.Duration) error {
	return errors.New("no TCP user timeout on this system")
}
