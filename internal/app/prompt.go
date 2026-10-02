// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// confirm asks on the terminal, because stdout and stderr may carry JSON logs.
func confirm(message string) error {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return errors.New("startup requires confirmation on a controlling terminal (/dev/tty); no dataset was initialized or recovered")
	}
	defer tty.Close()
	return confirmOn(tty, tty, message)
}
func confirmOn(r io.Reader, w io.Writer, message string) error {
	if _, err := fmt.Fprintf(w, "%s\nContinue? [yes/no]: ", message); err != nil {
		return err
	}
	line, err := bufio.NewReader(io.LimitReader(r, 1024)).ReadString('\n')
	if err != nil {
		return errors.New("confirmation unavailable or incomplete; startup cancelled")
	}
	if strings.TrimSpace(strings.ToLower(line)) != "yes" {
		return errors.New("startup cancelled; confirmation requires yes")
	}
	return nil
}
