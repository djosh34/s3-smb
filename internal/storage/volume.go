// SPDX-License-Identifier: AGPL-3.0-only
package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/djosh34/s3-smb/internal/config"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/meta"
	"github.com/djosh34/s3-smb/internal/juicefs/pkg/object"
	"github.com/djosh34/s3-smb/internal/logging"
	"github.com/google/uuid"
)

// VolumeName is the JuiceFS volume name and the bucket prefix of every object.
const VolumeName = "s3-smb"

const identityKey = VolumeName + "/format.json"
const keyPrefix = VolumeName + "/keys/"

var volumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$`)

func OpenS3(c *config.Resolved) (object.ObjectStorage, error) {
	return object.NewS3(object.S3Options{Bucket: c.S3.Bucket, Region: c.S3.Region, Endpoint: c.S3.Endpoint, AccessKey: c.AccessKey, SecretKey: c.SecretKey, SessionToken: c.SessionToken, PathStyle: c.S3.PathStyle, TLSConfig: c.TLSConfig})
}
func NewFormat(name string, encrypted bool, trashDays int) (*meta.Format, error) {
	f := &meta.Format{Name: name, UUID: uuid.NewString(), Storage: "s3", BlockSize: 4096, Compression: "none", TrashDays: trashDays, MetaVersion: 1, DirStats: true}
	if encrypted {
		f.EncryptAlgo = object.AES256GCM_RSA
	}
	if err := validateFormat(f); err != nil {
		return nil, err
	}
	return f, nil
}
func validateFormat(f *meta.Format) error {
	if f == nil || !volumeName.MatchString(f.Name) {
		return errors.New("invalid native volume name")
	}
	if _, err := uuid.Parse(f.UUID); err != nil {
		return errors.New("invalid native volume UUID")
	}
	if f.BlockSize <= 0 || f.BlockSize > 16384 || f.BlockSize&(f.BlockSize-1) != 0 {
		return errors.New("invalid native block size")
	}
	if f.Compression != "none" && f.Compression != "lz4" && f.Compression != "zstd" {
		return errors.New("unsupported native compression")
	}
	if f.EncryptAlgo != "" && f.EncryptAlgo != object.AES256GCM_RSA {
		return errors.New("unsupported volume encryption mode")
	}
	if f.EncryptAlgo == "" && f.EncryptKey != "" {
		return errors.New("inconsistent native encryption mode")
	}
	if f.TrashDays < 0 || f.Shards != 0 || f.KeyEncrypted {
		return errors.New("unsupported native format settings")
	}
	return nil
}
func readBounded(ctx context.Context, raw object.ObjectStorage, key string, max int64) ([]byte, error) {
	r, err := raw.Get(ctx, key, 0, -1)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(r, max+1))
	closeErr := r.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(data)) > max {
		return nil, errors.New("remote bootstrap object too large")
	}
	return data, nil
}

// publishExact is retry safe even when the PUT response is lost. It never
// overwrites or accepts another publisher's different bytes.
func publishExact(ctx context.Context, raw object.ObjectStorage, key string, data []byte) error {
	p, ok := raw.(interface {
		PutIfAbsent(context.Context, string, io.Reader) error
	})
	if !ok {
		return errors.New("object storage does not support conditional publication")
	}
	putErr := p.PutIfAbsent(ctx, key, bytes.NewReader(data))
	got, err := readBounded(ctx, raw, key, int64(len(data)))
	if err != nil {
		return fmt.Errorf("verify bootstrap publication: %w", err)
	}
	if !bytes.Equal(got, data) {
		return errors.New("bootstrap publication conflict; existing object preserved")
	}
	_ = putErr // exact readback resolves an ambiguous response or identical retry
	return nil
}
func ReadIdentity(ctx context.Context, raw object.ObjectStorage) (*meta.Format, error) {
	data, err := readBounded(ctx, raw, identityKey, 128*1024)
	if err != nil {
		return nil, err
	}
	var f meta.Format
	if err = json.Unmarshal(data, &f); err != nil {
		return nil, errors.New("invalid native volume identity")
	}
	if err = validateFormat(&f); err != nil {
		return nil, err
	}
	return &f, nil
}
func PublishIdentity(ctx context.Context, raw object.ObjectStorage, f *meta.Format) error {
	if err := validateFormat(f); err != nil {
		return err
	}
	copy := *f
	// Current validated configuration is the only destination/credential authority.
	copy.Bucket = ""
	copy.AccessKey = ""
	copy.SecretKey = ""
	copy.SessionToken = ""
	data, err := json.Marshal(&copy)
	if err != nil {
		return err
	}
	return publishExact(ctx, raw, identityKey, data)
}
func OpenVolume(ctx context.Context, raw object.ObjectStorage, f *meta.Format, passphrase string, create bool) (object.ObjectStorage, error) {
	if err := validateFormat(f); err != nil {
		return nil, err
	}
	blob := object.WithPrefix(raw, f.Name+"/")
	// The native CLI initializes tier zero even for the ordinary default store.
	// Preserve saved native tier settings rather than warning on every upload.
	if tiers, ok := raw.(object.SupportTier); ok {
		if err := tiers.InitTiers(f.Tiers); err != nil {
			return nil, err
		}
	}
	if f.EncryptAlgo == "" {
		return blob, nil
	}
	keyPath := keyPrefix + f.UUID + ".pem"
	data, err := readBounded(ctx, raw, keyPath, maxKeyBytes)
	if err != nil {
		if !create || f.EncryptKey != "" || !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read protected volume key: %w", err)
		}
		data, err = generateKey(passphrase)
		if err != nil {
			return nil, err
		}
		if err = publishExact(ctx, raw, keyPath, data); err != nil {
			return nil, err
		}
	}
	logging.RegisterSecret(string(data))
	key, err := unlockKey(data, passphrase)
	if err != nil {
		return nil, err
	}
	if f.EncryptKey != "" && f.EncryptKey != string(data) {
		return nil, errors.New("volume key does not match native format")
	}
	f.EncryptKey = string(data)
	enc, err := object.NewDataEncryptor(object.NewRSAEncryptor(key), f.EncryptAlgo)
	if err != nil {
		return nil, err
	}
	return object.NewEncrypted(blob, enc), nil
}
func VerifyMarker(ctx context.Context, blob object.ObjectStorage, f *meta.Format) error {
	data, err := readBounded(ctx, blob, "juicefs_uuid", 128)
	if err != nil {
		return err
	}
	if string(data) != f.UUID {
		return errors.New("native volume marker UUID mismatch")
	}
	return nil
}
func PublishMarker(ctx context.Context, blob object.ObjectStorage, f *meta.Format) error {
	return publishExact(ctx, blob, "juicefs_uuid", []byte(f.UUID))
}

// DiscoverRecoveryVolume only unlocks an existing, unique bootstrap key. Native
// backup inspection must validate the returned candidate against the export
// before loading it. Missing identity never implies a new dataset.
func DiscoverRecoveryVolume(ctx context.Context, raw object.ObjectStorage, encrypted bool, passphrase string) (object.ObjectStorage, *meta.Format, error) {
	f, err := NewFormat(VolumeName, encrypted, 14)
	if err != nil {
		return nil, nil, err
	}
	if !encrypted {
		return object.WithPrefix(raw, VolumeName+"/"), nil, nil
	}
	entries, more, _, err := raw.List(ctx, keyPrefix, "", "", "", 2, true)
	if err != nil {
		return nil, nil, err
	}
	if more || len(entries) != 1 {
		return nil, nil, errors.New("recovery requires exactly one existing volume key")
	}
	name := entries[0].Key()
	if !strings.HasPrefix(name, keyPrefix) || !strings.HasSuffix(name, ".pem") {
		return nil, nil, errors.New("unexpected bootstrap key object")
	}
	f.UUID = strings.TrimSuffix(strings.TrimPrefix(name, keyPrefix), ".pem")
	blob, err := OpenVolume(ctx, raw, f, passphrase, false)
	return blob, f, err
}
