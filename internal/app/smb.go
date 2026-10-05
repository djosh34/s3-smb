// SPDX-License-Identifier: AGPL-3.0-only
package app

import (
	"crypto/rand"
	"errors"
	"log/slog"
	"time"

	"github.com/djosh34/s3-smb/internal/config"
	"github.com/djosh34/s3-smb/internal/smb/auth"
	"github.com/djosh34/s3-smb/internal/smb/server"
	"github.com/djosh34/s3-smb/internal/smb/state"
	"github.com/djosh34/s3-smb/internal/smbfs"
	"github.com/djosh34/s3-smb/internal/storage"
)

func newSMBServer(runtime *storage.Runtime, c config.SMBConfig, metadataPath string) (smbServer, smbAdapter, error) {
	barrier, err := smbfs.NewMetadataBarrier(metadataPath)
	if err != nil {
		return nil, nil, err
	}
	adapter, err := smbfs.New(smbfs.Options{
		Filesystem: runtime.FS, Config: runtime.Config, Store: runtime.Store,
		Barrier: barrier, MetadataPath: metadataPath,
		Capacity: runtime.Config.Format.Capacity, ReadOnly: c.ReadOnly,
	})
	if err != nil {
		return nil, nil, err
	}
	table, err := state.New(time.Now)
	if err != nil {
		return nil, nil, errors.Join(err, adapter.Shutdown())
	}
	policy := server.RequireEncryption
	if !c.Encryption {
		policy = server.AllowPlaintext
	}
	var guid [16]byte
	if _, err = rand.Read(guid[:]); err != nil {
		return nil, nil, errors.Join(err, adapter.Shutdown())
	}
	s, err := server.New(server.Options{
		Storage: adapter, State: table, Logger: slog.Default(), Now: time.Now,
		Account:   auth.Account{User: c.Username, Password: c.Password},
		ShareName: c.Share, ServerName: "s3-smb", ServerGUID: guid, Encryption: policy,
	})
	if err != nil {
		return nil, nil, errors.Join(err, adapter.Shutdown())
	}
	return s, adapter, nil
}
