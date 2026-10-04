// SPDX-License-Identifier: AGPL-3.0-only
package helpers

import (
	"context"
	"errors"
	"time"
)

// Cleanup limits client cleanup, then always attempts unload with the original context.
func Cleanup(ctx context.Context, clientBudget time.Duration, clients, unload func(context.Context) error) error {
	clientCtx, cancel := context.WithTimeout(ctx, clientBudget)
	clientErr := clients(clientCtx)
	cancel()
	return errors.Join(clientErr, unload(ctx))
}
