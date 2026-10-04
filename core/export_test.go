package core

import "maps"

// SaveRegistry snapshots the package-global module registry and returns
// its restore — call as: t.Cleanup(SaveRegistry()), so a test that
// registers leaves the registry as it found it and -count=N reruns stay
// clean.
func SaveRegistry() func() {
	regMu.Lock()
	defer regMu.Unlock()
	saved := maps.Clone(reg)
	return func() {
		regMu.Lock()
		defer regMu.Unlock()
		reg = saved
	}
}
