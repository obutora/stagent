package loginenv

import (
	"errors"
	"time"
)

// Capture: Windows has no login shell; an sshd session already carries the
// user's registry environment (PATH included).
func Capture(string, time.Duration) ([]string, error) {
	return nil, errors.New("no login shell on windows")
}
