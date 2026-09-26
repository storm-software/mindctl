package headroom

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"

	"github.com/storm-software/mindctl/internal/config"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
)

type Process interface {
	Wait() error
	Kill() error
}

type ProcessRunner interface {
	Start(context.Context, string, []string, []string) (Process, error)
}

type execRunner struct{}

type execProcess struct{ command *exec.Cmd }

func (execRunner) Start(ctx context.Context, executable string, args []string, env []string) (Process, error) {
	command := exec.CommandContext(ctx, executable, args...)
	command.Env = env
	if err := command.Start(); err != nil {
		return nil, err
	}
	return execProcess{command: command}, nil
}

func (process execProcess) Wait() error { return process.command.Wait() }
func (process execProcess) Kill() error { return process.command.Process.Kill() }

func NewExecRunner() ProcessRunner { return execRunner{} }

type Manager struct {
	cfg         config.HeadroomConfig
	provisioner Provisioning
	httpClient  *http.Client
	runner      ProcessRunner

	mu       sync.RWMutex
	client   *Client
	process  Process
	server   *http.Server
	listener net.Listener
	token    string
	failure  error
	closed   bool
}

func NewManager(cfg config.HeadroomConfig, provisioner Provisioning, client *http.Client, runner ProcessRunner) *Manager {
	if client == nil {
		client = &http.Client{}
	}
	return &Manager{cfg: cfg, provisioner: provisioner, httpClient: client, runner: runner}
}

func (m *Manager) Start(ctx context.Context) error {
	if m == nil || !m.cfg.Enabled {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("headroom manager is closed")
	}
	if m.failure != nil {
		return m.failure
	}
	if m.client != nil {
		return nil
	}
	if m.provisioner == nil || m.runner == nil {
		m.failure = errors.New("headroom manager dependencies are unavailable")
		return m.failure
	}
	executable, err := m.provisioner.Ensure(ctx)
	if err != nil {
		m.failure = unavailable(errors.New("managed runtime unavailable"))
		return m.failure
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		m.failure = unavailable(errors.New("loopback listener unavailable"))
		return m.failure
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	token, err := randomToken()
	if err != nil {
		m.failure = unavailable(errors.New("sidecar token unavailable"))
		return m.failure
	}
	args := []string{"proxy", "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--mode", m.cfg.ModeValue(), "--no-ccr"}
	env := []string{"PATH=" + os.Getenv("PATH"), "HEADROOM_TELEMETRY=off", "HEADROOM_BEACON=off", "HEADROOM_LOG_MESSAGES=0"}
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		env = append(env, "HOME="+home)
	}
	process, err := m.runner.Start(ctx, executable, args, env)
	if err != nil {
		m.failure = unavailable(errors.New("managed runtime failed to start"))
		return m.failure
	}
	authListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = process.Kill()
		m.failure = unavailable(errors.New("authentication listener unavailable"))
		return m.failure
	}
	authServer := &http.Server{Handler: authenticatedProxy{token: token, target: "http://127.0.0.1:" + strconv.Itoa(port), client: m.httpClient}}
	go func() { _ = authServer.Serve(authListener) }()
	client := NewClient("http://"+authListener.Addr().String(), token, m.httpClient)
	if err := client.Ready(ctx); err != nil {
		_ = process.Kill()
		_ = authServer.Close()
		m.failure = unavailable(errors.New("managed runtime failed readiness"))
		return m.failure
	}
	m.client, m.process, m.server, m.listener, m.token = client, process, authServer, authListener, token
	go m.watch(process)
	return nil
}

func (m *Manager) watch(process Process) {
	if err := process.Wait(); err == nil {
		err = errors.New("managed runtime stopped")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.process == process && !m.closed {
		m.failure = unavailable(errors.New("managed runtime stopped"))
	}
}

func (m *Manager) Compress(ctx context.Context, model domain.Model, conversationID, providerID string, request inference.Request) (inference.Request, Metrics, error) {
	m.mu.RLock()
	client, failure, closed := m.client, m.failure, m.closed
	m.mu.RUnlock()
	if closed || failure != nil || client == nil {
		return inference.Request{}, Metrics{}, unavailable(errors.New("managed runtime unavailable"))
	}
	return client.Compress(ctx, model, conversationID, providerID, request)
}

func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	process := m.process
	server := m.server
	listener := m.listener
	m.mu.Unlock()
	if server != nil {
		_ = server.Close()
	}
	if listener != nil {
		_ = listener.Close()
	}
	if process != nil {
		return process.Kill()
	}
	return nil
}

type authenticatedProxy struct {
	token  string
	target string
	client *http.Client
}

func (proxy authenticatedProxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/v1/compress" || request.Header.Get("Authorization") != "Bearer "+proxy.token {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	forward, err := http.NewRequestWithContext(request.Context(), request.Method, proxy.target+request.URL.RequestURI(), request.Body)
	if err != nil {
		writer.WriteHeader(http.StatusBadGateway)
		return
	}
	forward.Header = request.Header.Clone()
	forward.Header.Del("Authorization")
	response, err := proxy.client.Do(forward)
	if err != nil {
		writer.WriteHeader(http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	for key, values := range response.Header {
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}
	writer.WriteHeader(response.StatusCode)
	_, _ = io.Copy(writer, response.Body)
}

func randomToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("random token: %w", err)
	}
	return hex.EncodeToString(value), nil
}

var _ Compressor = (*Manager)(nil)
