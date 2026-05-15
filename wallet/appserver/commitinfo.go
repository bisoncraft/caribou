// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package appserver

import "runtime/debug"

// commitHash is the VCS revision the binary was built from, or empty if the
// build info is unavailable.
var commitHash = func() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && len(setting.Value) >= 8 {
				return setting.Value
			}
		}
	}
	return ""
}()
