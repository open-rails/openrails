//go:build integration

package integrationharness

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
)

// StandaloneProcess is the actual `openrails run-server` binary running as a
// separate OS process over the shared database and Redis, on a fixed loopback
// port so a restart keeps the same URL. It is the deployment shape a
// self-hosting merchant operates; nothing in the test process shares memory
// with it.
type StandaloneProcess struct {
	BaseURL string

	h          *Harness
	binary     string
	configPath string
	logPath    string
	port       int

	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan error
}

// ProcessOption customizes the server's config file.
type ProcessOption func(*processConfig)

type processConfig struct {
	extraYAML []string
}

// ProcessWithYAML appends top-level YAML to the server's config file.
func ProcessWithYAML(yaml string) ProcessOption {
	return func(c *processConfig) { c.extraYAML = append(c.extraYAML, yaml) }
}

// ProcessWithNMIGateway points the server's sandbox NMI clients at a loopback
// gateway (config.ProviderSandbox).
func ProcessWithNMIGateway(url string) ProcessOption {
	return ProcessWithYAML("provider_sandbox:\n  nmi_gateway_url: " + url + "\n")
}

var (
	binaryOnce sync.Once
	binaryPath string
	binaryErr  error
)

// openrailsBinary builds cmd/openrails once per test process.
func openrailsBinary() (string, error) {
	binaryOnce.Do(func() {
		root, err := moduleRoot()
		if err != nil {
			binaryErr = err
			return
		}
		dir, err := os.MkdirTemp("", "openrails-bin-")
		if err != nil {
			binaryErr = err
			return
		}
		binaryPath = filepath.Join(dir, "openrails")
		build := exec.Command("go", "build", "-o", binaryPath, "./cmd/openrails")
		build.Dir = root
		build.Env = os.Environ()
		if out, err := build.CombinedOutput(); err != nil {
			binaryErr = fmt.Errorf("build cmd/openrails: %w\n%s", err, out)
		}
	})
	return binaryPath, binaryErr
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

// StartStandaloneProcess writes the server config, builds the binary and
// starts the process, waiting until it is live.
func (h *Harness) StartStandaloneProcess(opts ...ProcessOption) *StandaloneProcess {
	h.t.Helper()
	var pc processConfig
	for _, opt := range opts {
		if opt != nil {
			opt(&pc)
		}
	}
	binary, err := openrailsBinary()
	require.NoError(h.t, err)
	dbtest.EnsureTestMerchant(h.ctx, h.t, h.sharedPool())

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(h.t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(h.t, listener.Close())
	dir := h.t.TempDir()
	redisAddr := ""
	if h.Redis != nil {
		redisAddr = h.Redis.Options().Addr
	}
	cfgYAML := fmt.Sprintf(`env: dev
merchant_source: api
secret_backend: db
test_mode: sandbox
provider_write_mode: full
host: 127.0.0.1
port: %d
api_url: http://127.0.0.1:%d
db:
  url: %s
redis:
  addr: %s
auth:
  issuer: https://process.openrails.test
  keys_path: %s
%s`, port, port, h.DSN, redisAddr, filepath.Join(dir, "keys"), strings.Join(pc.extraYAML, ""))
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(h.t, os.MkdirAll(filepath.Join(dir, "keys"), 0o700))
	require.NoError(h.t, os.WriteFile(configPath, []byte(cfgYAML), 0o600))

	p := &StandaloneProcess{
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d", port), h: h, binary: binary,
		configPath: configPath, logPath: filepath.Join(dir, "server.log"), port: port,
	}
	p.Start()
	h.cleanup(p.Stop)
	return p
}

// Start launches `openrails run-server` and blocks until /health/live answers.
func (p *StandaloneProcess) Start() {
	h := p.h
	h.t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil {
		return
	}
	logFile, err := os.OpenFile(p.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(h.t, err)
	cmd := exec.Command(p.binary, "run-server", "--config", p.configPath)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "TMPDIR=" + os.TempDir()}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	require.NoError(h.t, cmd.Start(), "start openrails run-server")
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
		_ = logFile.Close()
	}()
	p.cmd, p.done = cmd, done

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			p.cmd, p.done = nil, nil
			h.t.Fatalf("run-server exited before becoming live: %v\n%s", err, p.logTail())
		default:
		}
		resp, err := http.Get(p.BaseURL + "/health/live")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	h.t.Fatalf("run-server did not become live\n%s", p.logTail())
}

// Stop sends SIGTERM and waits for the process to exit; a process that does
// not stop in time is killed and the test fails.
func (p *StandaloneProcess) Stop() {
	h := p.h
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-p.done:
		p.cmd, p.done = nil, nil
		if err != nil {
			h.t.Errorf("run-server did not shut down cleanly: %v\n%s", err, p.logTail())
		}
	case <-time.After(45 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
		p.cmd, p.done = nil, nil
		h.t.Errorf("run-server ignored SIGTERM and was killed\n%s", p.logTail())
	}
}

// Kill stops the process without a graceful shutdown (SIGKILL), the crash a
// restart must survive.
func (p *StandaloneProcess) Kill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	<-p.done
	p.cmd, p.done = nil, nil
}

func (p *StandaloneProcess) logTail() string {
	raw, err := os.ReadFile(p.logPath)
	if err != nil {
		return ""
	}
	if len(raw) > 8000 {
		raw = raw[len(raw)-8000:]
	}
	return string(raw)
}
