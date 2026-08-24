package cli

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
)

func TestBuildVersionSelectsVersionSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fallback string
		info     *debug.BuildInfo
		ok       bool
		want     string
	}{
		{
			name:     "source distribution uses injected fallback",
			fallback: "v0.1.1",
			want:     "v0.1.1",
		},
		{
			name:     "module version takes precedence",
			fallback: "v0.1.1",
			info:     &debug.BuildInfo{Main: debug.Module{Version: "v0.2.0"}},
			ok:       true,
			want:     "v0.2.0",
		},
		{
			name:     "development module version keeps injected fallback",
			fallback: "v0.1.1",
			info:     &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			ok:       true,
			want:     "v0.1.1",
		},
		{
			name: "empty fallback uses development version",
			want: devVersion,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildVersion(func() (*debug.BuildInfo, bool) {
				return tt.info, tt.ok
			}, tt.fallback)
			if got.Version != tt.want {
				t.Fatalf("Version = %q, want %q", got.Version, tt.want)
			}
		})
	}
}

func TestLinkerInjectedReleaseVersion(t *testing.T) {
	t.Parallel()

	const want = "v9.8.7"
	binaryName := "jira-cli"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binary := filepath.Join(t.TempDir(), binaryName)
	build := exec.Command(
		"go",
		"build",
		"-buildvcs=false",
		"-trimpath",
		"-ldflags=-X github.com/abigotado/jira-cli/internal/cli.releaseVersion="+want,
		"-o",
		binary,
		"./cmd/jira-cli",
	)
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI with injected release version: %v\n%s", err, output)
	}

	output, err := exec.Command(binary, "version", "-o", "json").Output()
	if err != nil {
		t.Fatalf("run built CLI version command: %v", err)
	}
	var response struct {
		OK   bool `json:"ok"`
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("decode version response: %v", err)
	}
	if !response.OK {
		t.Fatal("version response was not successful")
	}
	if response.Data.Version != want {
		t.Fatalf("Version = %q, want %q", response.Data.Version, want)
	}
}
