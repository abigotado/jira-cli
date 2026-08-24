package cli

import (
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

const devVersion = "devel"

// releaseVersion is set at link time for source distributions such as
// Homebrew. Module-aware builds continue to prefer the version recorded in
// Go build information.
var releaseVersion = devVersion

func (a *App) newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version and Go build stamp",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(_ *cobra.Command, _ []string) error {
			return a.out.Success(buildVersion(debug.ReadBuildInfo, releaseVersion))
		},
	}
}

func buildVersion(read func() (*debug.BuildInfo, bool), fallback string) versionView {
	if fallback == "" {
		fallback = devVersion
	}
	view := versionView{Version: fallback, Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	info, ok := read()
	if !ok {
		return view
	}
	if info.GoVersion != "" {
		view.Go = info.GoVersion
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		view.Version = info.Main.Version
	}
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			view.Commit = setting.Value
		case "vcs.time":
			view.CommitTime = setting.Value
		case "vcs.modified":
			if setting.Value == "true" {
				view.Version += "+dirty"
			}
		}
	}
	return view
}
