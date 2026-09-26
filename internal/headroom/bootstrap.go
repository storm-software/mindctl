package headroom

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// These files are carried by every native binary so provisioning never needs
// a companion checkout or mutable project configuration.
var (
	//go:embed assets/pyproject.toml
	embeddedProject []byte
	//go:embed assets/uv.lock
	embeddedLock []byte
	//go:embed assets/artifacts.json
	embeddedArtifacts []byte
)

// EmbeddedAssets reports the immutable provisioning payload carried by the
// executable. It is useful to release checks without exposing its contents.
func EmbeddedAssets() (project, lock, artifacts []byte) {
	return append([]byte(nil), embeddedProject...), append([]byte(nil), embeddedLock...), append([]byte(nil), embeddedArtifacts...)
}

// Runner executes a verified bootstrap utility with an explicit environment.
type Runner func(context.Context, string, []string, []string) error

// Provisioning supplies the verified managed Headroom executable.
type Provisioning interface {
	Ensure(context.Context) (string, error)
}

const (
	bootstrapVersion = "0.39.0-uv0.12.11-python3.13.15"
	maxDownloadBytes = 256 << 20
)

// Provisioner resolves and atomically installs a private, versioned runtime.
type Provisioner struct {
	cacheDir string
	client   *http.Client
	runner   Runner
}

func NewProvisioner(cacheDir string, client *http.Client, runner Runner) *Provisioner {
	return &Provisioner{cacheDir: cacheDir, client: client, runner: runner}
}

func (p *Provisioner) Ensure(ctx context.Context) (string, error) {
	if p == nil || p.cacheDir == "" {
		return "", errors.New("headroom runtime cache is not configured")
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	targetDir := filepath.Join(p.cacheDir, "headroom", bootstrapVersion, runtime.GOOS+"-"+runtime.GOARCH)
	executable := filepath.Join(targetDir, "bin", "headroom")
	if runtime.GOOS == "windows" {
		executable = filepath.Join(targetDir, "Scripts", "headroom.exe")
	}
	if info, err := os.Stat(executable); err == nil && !info.IsDir() {
		return executable, nil
	}
	if err := os.MkdirAll(filepath.Dir(targetDir), 0o700); err != nil {
		return "", errors.New("create Headroom cache failed")
	}
	if err := acquireLock(ctx, targetDir+".lock"); err != nil {
		return "", err
	}
	defer os.RemoveAll(targetDir + ".lock")
	if info, err := os.Stat(executable); err == nil && !info.IsDir() {
		return executable, nil
	}
	var manifest struct {
		Targets map[string]map[string]string `json:"targets"`
	}
	if err := json.Unmarshal(embeddedArtifacts, &manifest); err != nil {
		return "", errors.New("invalid embedded Headroom manifest")
	}
	platform, ok := manifest.Targets[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok || platform["uv_url"] == "" || platform["uv_sha256"] == "" {
		return "", errors.New("no verified Headroom runtime for this platform")
	}
	client := p.client
	if client == nil {
		client = http.DefaultClient
	}
	archive, err := downloadVerified(ctx, client, platform["uv_url"], platform["uv_sha256"])
	if err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(filepath.Dir(targetDir), ".headroom-stage-")
	if err != nil {
		return "", errors.New("create Headroom staging directory failed")
	}
	defer os.RemoveAll(stage)
	uvPath, err := extractUV(archive, stage)
	if err != nil {
		return "", err
	}
	projectDir := filepath.Join(stage, "project")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		return "", errors.New("create Headroom project directory failed")
	}
	for name, contents := range map[string][]byte{"pyproject.toml": embeddedProject, "uv.lock": embeddedLock} {
		if err := os.WriteFile(filepath.Join(projectDir, name), contents, 0o600); err != nil {
			return "", errors.New("write embedded Headroom project failed")
		}
	}
	runner := p.runner
	if runner == nil {
		runner = defaultRunner
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "UV_CACHE_DIR=" + filepath.Join(p.cacheDir, "uv"), "UV_PYTHON_INSTALL_DIR=" + filepath.Join(p.cacheDir, "python"), "UV_PYTHON_DOWNLOADS=manual"}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		env = append(env, "HOME="+home)
	}
	if err := runner(ctx, uvPath, []string{"python", "install", "3.13.15"}, env); err != nil {
		return "", errors.New("install pinned Python runtime failed")
	}
	if err := runner(ctx, uvPath, []string{"sync", "--locked", "--project", projectDir, "--python", "3.13.15"}, env); err != nil {
		return "", errors.New("install verified Headroom environment failed")
	}
	venvExecutable := filepath.Join(projectDir, ".venv", "bin", "headroom")
	if runtime.GOOS == "windows" {
		venvExecutable = filepath.Join(projectDir, ".venv", "Scripts", "headroom.exe")
	}
	if info, err := os.Stat(venvExecutable); err != nil || info.IsDir() {
		return "", errors.New("Headroom proxy executable was not installed")
	}
	if err := os.MkdirAll(filepath.Dir(targetDir), 0o700); err != nil {
		return "", errors.New("create Headroom cache failed")
	}
	if err := os.Rename(filepath.Join(projectDir, ".venv"), targetDir); err != nil {
		return "", errors.New("publish Headroom runtime failed")
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(executable, 0o700)
		if err := repairUnixLauncher(executable, filepath.Join(targetDir, "bin", "python")); err != nil {
			return "", errors.New("repair Headroom launcher failed")
		}
	}
	return executable, nil
}

func repairUnixLauncher(path, python string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lineEnd := bytes.IndexByte(data, '\n')
	if lineEnd < 0 || !bytes.HasPrefix(data, []byte("#!")) {
		return errors.New("invalid Headroom launcher")
	}
	repaired := append([]byte("#!"+python), data[lineEnd:]...)
	return os.WriteFile(path, repaired, 0o700)
}

func defaultRunner(ctx context.Context, executable string, args []string, env []string) error {
	command := exec.CommandContext(ctx, executable, args...)
	command.Env = env
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Run()
}

func acquireLock(ctx context.Context, path string) error {
	for {
		err := os.Mkdir(path, 0o700)
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrExist) {
			return errors.New("create Headroom install lock failed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func downloadVerified(ctx context.Context, client *http.Client, url, expected string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.New("invalid Headroom artifact URL")
	}
	response, err := client.Do(request)
	if err != nil || response.StatusCode != http.StatusOK {
		if response != nil {
			_ = response.Body.Close()
		}
		return nil, errors.New("download verified Headroom artifact failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDownloadBytes+1))
	if err != nil || len(data) > maxDownloadBytes {
		return nil, errors.New("Headroom artifact is too large")
	}
	digest := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), expected) {
		return nil, errors.New("Headroom artifact checksum mismatch")
	}
	return data, nil
}

func extractUV(archive []byte, destination string) (string, error) {
	if runtime.GOOS == "windows" {
		reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return "", errors.New("invalid uv archive")
		}
		for _, file := range reader.File {
			if !strings.HasSuffix(file.Name, "/uv.exe") && file.Name != "uv.exe" {
				continue
			}
			return extractZipFile(file, destination, "uv.exe")
		}
	} else {
		reader, err := gzip.NewReader(bytes.NewReader(archive))
		if err != nil {
			return "", errors.New("invalid uv archive")
		}
		defer reader.Close()
		entries := tar.NewReader(reader)
		for {
			header, err := entries.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return "", errors.New("invalid uv archive")
			}
			if filepath.Base(header.Name) != "uv" || header.Typeflag != tar.TypeReg {
				continue
			}
			path := filepath.Join(destination, "uv")
			file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
			if err != nil {
				return "", errors.New("write uv executable failed")
			}
			_, copyErr := io.Copy(file, io.LimitReader(entries, maxDownloadBytes))
			closeErr := file.Close()
			if copyErr != nil || closeErr != nil {
				return "", errors.New("write uv executable failed")
			}
			return path, nil
		}
	}
	return "", errors.New("uv executable missing from verified archive")
}

func extractZipFile(file *zip.File, destination, name string) (string, error) {
	reader, err := file.Open()
	if err != nil {
		return "", errors.New("read uv archive failed")
	}
	defer reader.Close()
	path := filepath.Join(destination, name)
	output, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		return "", errors.New("write uv executable failed")
	}
	_, copyErr := io.Copy(output, io.LimitReader(reader, maxDownloadBytes))
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		return "", errors.New("write uv executable failed")
	}
	return path, nil
}

var _ Provisioning = (*Provisioner)(nil)
