// Modified for s3-smb, 2026. See docs/vendored.md.

package smb2

import (
	"github.com/djosh34/s3-smb/internal/logging"
	logrus "github.com/sirupsen/logrus"
)

// Install before any server construction; slog configuration remains dynamic.
var log logrus.FieldLogger = newNativeLogger()

func newNativeLogger() *logrus.Logger {
	l := logrus.New()
	logging.Logrus(l, "smb")
	return l
}

func SetLogger(logger logrus.FieldLogger) {
	if logger != nil {
		log = logger
	}
}
