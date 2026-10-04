//go:build !darwin

package smbfs

import "os"

func syncFile(file *os.File, _ bool) error { return file.Sync() }
