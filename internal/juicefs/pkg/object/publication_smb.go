// SPDX-License-Identifier: AGPL-3.0-only
// Conditional publication through ordinary native prefix/encryption wrappers.
package object

import (
	"bytes"
	"context"
	"fmt"
	"io"
)

type conditionalPublisher interface {
	PutIfAbsent(context.Context, string, io.Reader) error
}

func (p *withPrefix) PutIfAbsent(ctx context.Context, key string, in io.Reader) error {
	s, ok := p.os.(conditionalPublisher)
	if !ok {
		return fmt.Errorf("object store does not support conditional publication")
	}
	return s.PutIfAbsent(ctx, p.prefix+key, in)
}
func (e *encrypted) PutIfAbsent(ctx context.Context, key string, in io.Reader) error {
	s, ok := e.ObjectStorage.(conditionalPublisher)
	if !ok {
		return fmt.Errorf("object store does not support conditional publication")
	}
	plain, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	encrypted, err := e.enc.Encrypt(plain)
	if err != nil {
		return err
	}
	return s.PutIfAbsent(ctx, key, bytes.NewReader(encrypted))
}
