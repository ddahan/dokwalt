// Package sshx opens one SSH connection to a server and multiplexes
// everything over it: API calls (through `dokwalt dial-stdio`), tunnels,
// uploads and interactive commands.
package sshx

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/term"
)

// Settings resolved from `ssh -G`, so ~/.ssh/config (aliases, users, ports,
// identity files, ProxyJump) applies exactly as with the ssh command.
type Settings struct {
	Host           string
	User           string
	Port           string
	IdentityFiles  []string
	KnownHosts     []string
	ProxyJump      string
	StrictHostKeys string
	IdentitiesOnly bool
}

func Resolve(target string) (Settings, error) {
	s := Settings{Port: "22"}
	args := []string{"-G"}
	if cfg := os.Getenv("DOKWALT_SSH_CONFIG"); cfg != "" {
		args = append(args, "-F", cfg)
	}
	// user@host:port (ssh itself doesn't accept the :port suffix).
	if at := strings.LastIndex(target, "@"); strings.Count(target[at+1:], ":") == 1 {
		host, port, _ := strings.Cut(target[at+1:], ":")
		if _, err := strconv.Atoi(port); err == nil {
			target = target[:at+1] + host
			args = append(args, "-p", port)
		}
	}
	out, err := exec.Command("ssh", append(args, target)...).Output()
	if err != nil {
		// No ssh binary: parse user@host:port ourselves.
		user, host, _ := strings.Cut(target, "@")
		if host == "" {
			host, user = user, os.Getenv("USER")
		}
		if h, p, err := net.SplitHostPort(host); err == nil {
			host, s.Port = h, p
		}
		home, _ := os.UserHomeDir()
		s.Host, s.User = host, user
		s.IdentityFiles = []string{home + "/.ssh/id_ed25519", home + "/.ssh/id_ecdsa", home + "/.ssh/id_rsa"}
		s.KnownHosts = []string{home + "/.ssh/known_hosts"}
		return s, nil
	}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), " ")
		switch k {
		case "hostname":
			s.Host = v
		case "user":
			s.User = v
		case "port":
			s.Port = v
		case "identityfile":
			s.IdentityFiles = append(s.IdentityFiles, expand(v))
		case "userknownhostsfile":
			for _, f := range strings.Fields(v) {
				s.KnownHosts = append(s.KnownHosts, expand(f))
			}
		case "proxyjump":
			if v != "none" {
				s.ProxyJump = v
			}
		case "stricthostkeychecking":
			s.StrictHostKeys = v
		case "identitiesonly":
			s.IdentitiesOnly = v == "yes"
		}
	}
	return s, nil
}

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

// Client wraps an SSH connection.
type Client struct {
	*ssh.Client
	Target string
	// Prompt asks the user a yes/no question (host key trust). Nil = refuse.
	Prompt func(question string) bool
}

// Dial connects to target ("user@host", "host:port" or an ssh config alias).
func Dial(target string, prompt func(string) bool) (*Client, error) {
	s, err := Resolve(target)
	if err != nil {
		return nil, err
	}
	auth, closeAgent := authMethods(s)
	defer closeAgent()
	cfg := &ssh.ClientConfig{
		User:            s.User,
		Auth:            auth,
		HostKeyCallback: hostKeyCallback(s, prompt),
		Timeout:         15 * time.Second,
	}
	addr := net.JoinHostPort(s.Host, s.Port)
	var conn *ssh.Client
	if s.ProxyJump != "" {
		jump, err := Dial(strings.Split(s.ProxyJump, ",")[0], prompt)
		if err != nil {
			return nil, fmt.Errorf("proxy jump %s: %w", s.ProxyJump, err)
		}
		nc, err := jump.Dial("tcp", addr)
		if err != nil {
			return nil, err
		}
		c, chans, reqs, err := ssh.NewClientConn(nc, addr, cfg)
		if err != nil {
			return nil, wrapAuthErr(err, s)
		}
		conn = ssh.NewClient(c, chans, reqs)
	} else {
		conn, err = ssh.Dial("tcp", addr, cfg)
		if err != nil {
			return nil, wrapAuthErr(err, s)
		}
	}
	c := &Client{Client: conn, Target: target, Prompt: prompt}
	go c.keepalive()
	return c, nil
}

func wrapAuthErr(err error, s Settings) error {
	if strings.Contains(err.Error(), "unable to authenticate") {
		return fmt.Errorf("SSH authentication to %s@%s failed — load your key with `ssh-add` or check that `ssh %s@%s` works: %w", s.User, s.Host, s.User, s.Host, err)
	}
	return err
}

func (c *Client) keepalive() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
			return
		}
	}
}

var passphraseMu sync.Mutex

// authMethods returns a single publickey method: x/crypto/ssh won't try a
// second "publickey" method after the first fails, so agent keys and
// identity files must be offered through one callback.
func authMethods(s Settings) ([]ssh.AuthMethod, func()) {
	var ag agent.ExtendedAgent
	closer := func() {}
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			ag = agent.NewClient(conn)
			closer = func() { conn.Close() }
		}
	}
	cb := func() ([]ssh.Signer, error) {
		var fileSigners []ssh.Signer
		wanted := map[string]bool{}
		for _, f := range s.IdentityFiles {
			if pub, err := os.ReadFile(f + ".pub"); err == nil {
				if pk, _, _, _, err := ssh.ParseAuthorizedKey(pub); err == nil {
					wanted[string(pk.Marshal())] = true
				}
			}
		}
		var agentSigners []ssh.Signer
		if ag != nil {
			if list, err := ag.Signers(); err == nil {
				for _, sg := range list {
					if !s.IdentitiesOnly || wanted[string(sg.PublicKey().Marshal())] {
						agentSigners = append(agentSigners, sg)
					}
				}
			}
		}
		inAgent := map[string]bool{}
		for _, sg := range agentSigners {
			inAgent[string(sg.PublicKey().Marshal())] = true
		}
		for _, f := range s.IdentityFiles {
			if pub, err := os.ReadFile(f + ".pub"); err == nil {
				if pk, _, _, _, err := ssh.ParseAuthorizedKey(pub); err == nil && inAgent[string(pk.Marshal())] {
					continue // the agent already offers this key
				}
			}
			if sg := loadKey(f); sg != nil {
				fileSigners = append(fileSigners, sg)
			}
		}
		return append(agentSigners, fileSigners...), nil
	}
	return []ssh.AuthMethod{ssh.PublicKeysCallback(cb)}, closer
}

func loadKey(f string) ssh.Signer {
	b, err := os.ReadFile(f)
	if err != nil {
		return nil
	}
	signer, err := ssh.ParsePrivateKey(b)
	var ppErr *ssh.PassphraseMissingError
	if errors.As(err, &ppErr) {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return nil
		}
		passphraseMu.Lock()
		fmt.Fprintf(os.Stderr, "Passphrase for %s: ", f)
		pass, perr := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		passphraseMu.Unlock()
		if perr != nil {
			return nil
		}
		signer, err = ssh.ParsePrivateKeyWithPassphrase(b, pass)
	}
	if err != nil {
		return nil
	}
	return signer
}

func hostKeyCallback(s Settings, prompt func(string) bool) ssh.HostKeyCallback {
	var files []string
	for _, f := range s.KnownHosts {
		if _, err := os.Stat(f); err == nil {
			files = append(files, f)
		}
	}
	var cb ssh.HostKeyCallback
	if len(files) > 0 {
		cb, _ = knownhosts.New(files...)
	}
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		if cb != nil {
			err := cb(hostname, remote, key)
			var ke *knownhosts.KeyError
			if err == nil {
				return nil
			}
			if !errors.As(err, &ke) || len(ke.Want) > 0 {
				// Known host with a different key: never continue.
				return fmt.Errorf("HOST KEY MISMATCH for %s — possible man-in-the-middle attack, or the server was reinstalled (then remove the old key with `ssh-keygen -R %s`): %w", hostname, s.Host, err)
			}
		}
		if s.StrictHostKeys == "yes" {
			return fmt.Errorf("unknown host %s and StrictHostKeyChecking=yes", hostname)
		}
		fp := ssh.FingerprintSHA256(key)
		trusted := s.StrictHostKeys == "accept-new" || s.StrictHostKeys == "no" || s.StrictHostKeys == "false"
		if !trusted && (prompt == nil || !prompt(fmt.Sprintf("The authenticity of host %s can't be established.\n  %s key fingerprint is %s.\n  Trust it and add it to known_hosts?", hostname, key.Type(), fp))) {
			return fmt.Errorf("host key for %s not trusted", hostname)
		}
		kh := s.KnownHosts
		if len(kh) == 0 {
			home, _ := os.UserHomeDir()
			kh = []string{home + "/.ssh/known_hosts"}
		}
		_ = os.MkdirAll(filepath.Dir(kh[0]), 0o700)
		f, err := os.OpenFile(kh[0], os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = fmt.Fprintln(f, knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key))
		return err
	}
}

// stdioConn adapts a session's stdin/stdout to net.Conn.
type stdioConn struct {
	sess   *ssh.Session
	stdin  io.WriteCloser
	stdout io.Reader
	once   sync.Once
}

func (c *stdioConn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *stdioConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }
func (c *stdioConn) Close() error {
	c.once.Do(func() {
		c.stdin.Close()
		c.sess.Close()
	})
	return nil
}
func (c *stdioConn) CloseWrite() error                { return c.stdin.Close() }
func (c *stdioConn) LocalAddr() net.Addr              { return dummyAddr{} }
func (c *stdioConn) RemoteAddr() net.Addr             { return dummyAddr{} }
func (c *stdioConn) SetDeadline(time.Time) error      { return nil }
func (c *stdioConn) SetReadDeadline(time.Time) error  { return nil }
func (c *stdioConn) SetWriteDeadline(time.Time) error { return nil }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "ssh" }
func (dummyAddr) String() string  { return "ssh-stdio" }

// Exec starts a remote command and returns its stdio as a net.Conn.
func (c *Client) Exec(cmd string) (net.Conn, error) {
	sess, err := c.NewSession()
	if err != nil {
		return nil, err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	if err := sess.Start(cmd); err != nil {
		sess.Close()
		return nil, err
	}
	return &stdioConn{sess: sess, stdin: stdin, stdout: &errOnEOF{r: stdout, stderr: &stderr}}, nil
}

// errOnEOF turns an immediate EOF with stderr output into a readable error
// (e.g. "dokwalt: command not found").
type errOnEOF struct {
	r      io.Reader
	stderr *bytes.Buffer
	got    bool
}

func (e *errOnEOF) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if n > 0 {
		e.got = true
	}
	if err == io.EOF && !e.got && e.stderr.Len() > 0 {
		time.Sleep(50 * time.Millisecond)
		return n, fmt.Errorf("remote: %s", strings.TrimSpace(e.stderr.String()))
	}
	return n, err
}

// Output runs a command and returns stdout, with stderr in the error.
func (c *Client) Output(cmd string) (string, error) {
	sess, err := c.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	var out, errb bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errb
	if err := sess.Run(cmd); err != nil {
		return out.String(), fmt.Errorf("%s: %w: %s", cmd, err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Stream runs a command and copies its stdout to w. There is no PTY, so
// binary output (e.g. pg_dump) arrives intact. Stderr goes in the error.
func (c *Client) Stream(cmd string, w io.Writer) error {
	sess, err := c.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	var errb bytes.Buffer
	sess.Stdout, sess.Stderr = w, &errb
	if err := sess.Run(cmd); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// Upload writes a local file to a remote path (via `cat`, no SFTP needed).
func (c *Client) Upload(local, remote string, mode os.FileMode, progress func(n int64)) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	sess, err := c.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	var errb bytes.Buffer
	sess.Stderr = &errb
	w, err := sess.StdinPipe()
	if err != nil {
		return err
	}
	cmd := fmt.Sprintf("cat > %s && chmod %o %s", shellQuote(remote), mode, shellQuote(remote))
	if err := sess.Start(cmd); err != nil {
		return err
	}
	var r io.Reader = f
	if progress != nil {
		r = &countingReader{r: f, fn: progress}
	}
	if _, err := io.Copy(w, r); err != nil {
		return err
	}
	w.Close()
	if err := sess.Wait(); err != nil {
		return fmt.Errorf("upload: %w: %s", err, errb.String())
	}
	return nil
}

type countingReader struct {
	r  io.Reader
	n  int64
	fn func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	c.fn(c.n)
	return n, err
}

// Interactive runs a command with a PTY bound to the local terminal
// (used for sudo prompts, `dokwalt exec`).
func (c *Client) Interactive(cmd string) error {
	sess, err := c.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	sess.Stdin, sess.Stdout, sess.Stderr = os.Stdin, os.Stdout, os.Stderr
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		w, h, _ := term.GetSize(fd)
		if w == 0 {
			w, h = 80, 24
		}
		termName := os.Getenv("TERM")
		if termName == "" {
			termName = "xterm-256color"
		}
		if err := sess.RequestPty(termName, h, w, ssh.TerminalModes{ssh.ECHO: 1}); err != nil {
			return err
		}
		old, err := term.MakeRaw(fd)
		if err == nil {
			defer term.Restore(fd, old)
		}
	}
	return sess.Run(cmd)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellQuote is exported for callers building remote commands.
func ShellQuote(s string) string { return shellQuote(s) }
