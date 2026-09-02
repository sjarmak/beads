package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPRCoreForwardsConfigurablePackageTimeout(t *testing.T) {
	repo := sourceRepoRoot(t)
	fakeBin := t.TempDir()
	fakeGo := filepath.Join(fakeBin, "go")

	if err := os.WriteFile(fakeGo, []byte("#!/usr/bin/env bash\nprintf '%s\\n' \"$*\" >\"$GO_ARGS_FILE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name        string
		timeoutEnv  string
		wantTimeout string
	}{
		{name: "default", wantTimeout: "25m"},
		{name: "override", timeoutEnv: "37m", wantTimeout: "37m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argsFile := filepath.Join(t.TempDir(), "go-args")
			cmd := exec.Command("bash", filepath.Join(repo, "scripts", "ci", "pr-core.sh"))
			cmd.Dir = repo
			cmd.Env = []string{
				"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
				"BEADS_TEST_ENV_DISABLE=1",
				"GO_ARGS_FILE=" + argsFile,
				"GO_TEST_PKG_PARALLEL=2",
				"GO_TEST_PARALLEL=3",
			}
			if tc.timeoutEnv != "" {
				cmd.Env = append(cmd.Env, "TEST_TIMEOUT="+tc.timeoutEnv)
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("pr-core.sh failed: %v\n%s", err, out)
			}

			data, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.TrimSpace(string(data))
			want := "test -p 2 -parallel 3 -timeout " + tc.wantTimeout + " -race -short -skip ^TestEmbedded ./..."
			if got != want {
				t.Fatalf("go invocation = %q, want %q", got, want)
			}
		})
	}
}
