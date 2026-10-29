// Package buildinfo exposes immutable release identity injected by the
// canonical container build. Development builds intentionally use explicit
// sentinel values instead of reading a mutable .git directory at runtime.
package buildinfo

type Info struct {
	Service   string `json:"service"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	Source    string `json:"source"`
}

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
	Source    = "unknown"
)

func Current() Info {
	return Info{
		Service: "ferry", Version: Version, Commit: Commit,
		BuildTime: BuildTime, Source: Source,
	}
}
