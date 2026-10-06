package dozor_test

import (
	project "dozor"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEmbeddedProjectVersion(t *testing.T) {
	out, err := exec.Command("bash", "scripts/version.sh").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := project.Version(), strings.TrimSpace(string(out)); got != want {
		t.Fatalf("binary version = %q, release version = %q", got, want)
	}
}

func TestReleaseVersionValidation(t *testing.T) {
	script, err := os.ReadFile("scripts/version.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, version string
		args          []string
		ok            bool
	}{
		{"project version", "v1.2.3\n", nil, true},
		{"matching tag", "v1.2.3\n", []string{"v1.2.3"}, true},
		{"tag mismatch", "v1.2.3\n", []string{"v0.1.0"}, false},
		{"extra arguments", "v1.2.3\n", []string{"v1.2.3", "v1.2.3"}, false},
		{"prerelease", "v1.2.3-beta.1\n", nil, false},
		{"leading zero", "v01.2.3\n", nil, false},
		{"missing prefix", "1.2.3\n", nil, false},
		{"empty version", "\n", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "scripts"), 0755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "scripts", "version.sh")
			if err := os.WriteFile(path, script, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte(tc.version), 0644); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("bash", append([]string{path}, tc.args...)...).CombinedOutput()
			if (err == nil) != tc.ok {
				t.Fatalf("version check: %v, %s", err, out)
			}
		})
	}
}
