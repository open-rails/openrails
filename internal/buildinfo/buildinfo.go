// Package buildinfo identifies the running binary. Release builds stamp the
// variables with -ldflags -X (.goreleaser.yaml, Dockerfile); any other build
// falls back to the module and VCS metadata the Go toolchain embeds.
package buildinfo

import (
	"cmp"
	"fmt"
	"runtime/debug"
)

var (
	version string
	commit  string
	date    string
)

// Info is the binary's version, source commit and commit time.
type Info struct {
	Version string
	Commit  string
	Date    string
}

// Get returns the stamped build info, filling gaps from debug.ReadBuildInfo.
func Get() Info {
	info := Info{Version: version, Commit: commit, Date: date}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if info.Version == "" && bi.Main.Version != "(devel)" {
			info.Version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && info.Commit == "" {
				info.Commit = s.Value
			}
			if s.Key == "vcs.time" && info.Date == "" {
				info.Date = s.Value
			}
		}
	}
	return Info{
		Version: cmp.Or(info.Version, "dev"),
		Commit:  cmp.Or(info.Commit, "unknown"),
		Date:    cmp.Or(info.Date, "unknown"),
	}
}

func (i Info) String() string {
	return fmt.Sprintf("openrails %s (commit %s, %s)", i.Version, i.Commit, i.Date)
}
