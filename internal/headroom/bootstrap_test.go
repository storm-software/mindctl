package headroom

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
)

func TestEmbeddedAssetsCoverReleaseTargets(t *testing.T) {
	project, lock, artifacts := EmbeddedAssets()
	if len(project) == 0 || len(lock) == 0 || len(artifacts) == 0 {
		t.Fatal("embedded provisioning assets are incomplete")
	}
	var manifest struct {
		Targets map[string]map[string]string `json:"targets"`
	}
	if err := json.Unmarshal(artifacts, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64", "windows/amd64"} {
		values, ok := manifest.Targets[target]
		if !ok || values["uv_url"] == "" || values["uv_sha256"] == "" || values["wheel_url"] == "" || values["wheel_sha256"] == "" {
			t.Fatalf("release manifest missing verified target %s", target)
		}
	}
}

func TestProvisionerRealColdStart(t *testing.T) {
	if os.Getenv("MINDCTL_HEADROOM_BOOTSTRAP") != "1" {
		t.Skip("set MINDCTL_HEADROOM_BOOTSTRAP=1 to run the pinned network bootstrap")
	}
	provisioner := NewProvisioner(t.TempDir(), &http.Client{}, nil)
	executable, err := provisioner.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(executable); err != nil {
		t.Fatal(err)
	}
	warm, err := provisioner.Ensure(context.Background())
	if err != nil || warm != executable {
		t.Fatalf("warm Ensure() = %q, %v; want %q", warm, err, executable)
	}
}
