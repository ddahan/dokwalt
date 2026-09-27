package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// composeFile writes a compose document to the app directory and returns its path.
func (e *Engine) composeFile(app, stage, project string, doc map[string]any) (string, error) {
	dir := filepath.Join(e.DataDir, "apps", app, stage)
	if app == "" {
		dir = filepath.Join(e.DataDir, "system")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, project+".json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o640); err != nil {
		return "", err
	}
	return path, os.Rename(tmp, path)
}

// compose runs `docker compose -p project -f file args...`. Config values are
// passed through the process environment only (never written to disk), which
// is also what `${VAR}` interpolation and pass-through env entries read.
func (e *Engine) compose(ctx context.Context, project, file string, env map[string]string, out func(string), args ...string) error {
	full := append([]string{"compose", "--progress", "plain", "-p", project}, args...)
	if file != "" {
		full = append([]string{"compose", "--progress", "plain", "-p", project, "-f", file}, args...)
	}
	cmd := exec.CommandContext(ctx, e.DockerBin, full...)
	cmd.Dir = filepath.Dir(firstNonEmpty(file, e.DataDir+"/"))
	cmd.Env = baseEnv()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	var tail []string
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			mu.Lock()
			tail = append(tail, line)
			if len(tail) > 20 {
				tail = tail[1:]
			}
			mu.Unlock()
			if out != nil {
				out(line)
			}
		}
	}()
	err := cmd.Run()
	pw.Close()
	<-done
	if err != nil {
		mu.Lock()
		defer mu.Unlock()
		return fmt.Errorf("docker compose %s failed: %w\n%s", args[0], err, strings.Join(tail, "\n"))
	}
	return nil
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// baseEnv keeps only what docker compose needs from the daemon environment.
func baseEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "PATH", "HOME", "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY", "TMPDIR", "LANG":
			env = append(env, kv)
		}
	}
	if os.Getenv("HOME") == "" {
		env = append(env, "HOME=/root")
	}
	if os.Getenv("PATH") == "" {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	return env
}
