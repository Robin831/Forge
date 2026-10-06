//go:build windows

package temper

import "context"

// acquireHeavyLock is a no-op on Windows: the lock exists for the Linux forge container's memory
// limit, and no Windows install sets FORGE_DOTNET_LOCK.
func acquireHeavyLock(_ context.Context, _ Step) (release func(), held bool) {
	return func() {}, false
}
