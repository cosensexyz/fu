package engine

import "runtime"

// keepDescriptorOwnersAlive is deferred around raw descriptor syscalls when
// no deferred Close otherwise proves the owning Go object remains reachable.
func keepDescriptorOwnersAlive(owners ...any) {
	for _, owner := range owners {
		runtime.KeepAlive(owner)
	}
}
