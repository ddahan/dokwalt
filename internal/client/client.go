// Package client talks to a DokWalt daemon, over SSH or a local socket.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dokwalt/dokwalt/internal/api"
	"github.com/dokwalt/dokwalt/internal/sshx"
)

const DefaultRemoteBin = "/usr/local/bin/dokwalt"

type Client struct {
	SSH       *sshx.Client // nil when talking to a local socket
	RemoteBin string
	http      *http.Client
	stream    *http.Client
}

// Local connects to a daemon socket on this machine.
func Local(socket string) *Client {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}
	return newClient(nil, "", dial)
}

// Remote connects through an SSH connection. Each HTTP connection is a
// `dokwalt dial-stdio` session on the server bridged to the daemon socket.
func Remote(c *sshx.Client, remoteBin string) *Client {
	if remoteBin == "" {
		remoteBin = DefaultRemoteBin
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		return c.Exec(remoteBin + " dial-stdio")
	}
	return newClient(c, remoteBin, dial)
}

func newClient(s *sshx.Client, bin string, dial func(context.Context, string, string) (net.Conn, error)) *Client {
	return &Client{
		SSH: s, RemoteBin: bin,
		http:   &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{DialContext: dial, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second}},
		stream: &http.Client{Transport: &http.Transport{DialContext: dial, DisableKeepAlives: true}},
	}
}

// APIError is a structured error returned by the daemon.
type APIError struct {
	Status int
	Body   api.Error
}

func (e *APIError) Error() string { return e.Body.Error }

// Hint returns the daemon's suggestion, if any.
func (e *APIError) Hint() string { return e.Body.Hint }

func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, hc *http.Client, method, path string, body io.Reader, ctype string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://dokwalt"+path, body)
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, friendly(err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		ae := &APIError{Status: resp.StatusCode}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(b, &ae.Body) != nil || ae.Body.Error == "" {
			ae.Body.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		}
		return nil, ae
	}
	return resp, nil
}

func friendly(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "command not found") || strings.Contains(msg, "No such file"):
		return fmt.Errorf("DokWalt is not installed on the server — run `dokwalt server init`: %w", err)
	case strings.Contains(msg, "permission denied"):
		return fmt.Errorf("permission denied on the DokWalt socket — is your SSH user in the `dokwalt` group? (log out and in again after `server init`): %w", err)
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such file or directory"):
		return fmt.Errorf("the DokWalt daemon is not running on the server (`sudo systemctl status dokwalt`): %w", err)
	}
	return err
}

func (c *Client) JSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	ctype := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ctype = bytes.NewReader(b), "application/json"
	}
	resp, err := c.do(ctx, c.http, method, path, body, ctype)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.JSON(ctx, "GET", path, nil, out)
}

func (c *Client) Post(ctx context.Context, path string, in, out any) error {
	return c.JSON(ctx, "POST", path, in, out)
}

func (c *Client) Delete(ctx context.Context, path string, out any) error {
	return c.JSON(ctx, "DELETE", path, nil, out)
}

// Maybe streams a response that may be either NDJSON events (long
// operation) or a plain JSON object (nothing to do). onEvent gets every
// event; the final "error" event is returned as an error.
func (c *Client) Operation(ctx context.Context, method, path string, in any, onEvent func(api.Event)) (api.Event, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return api.Event{}, err
		}
		body = bytes.NewReader(b)
	}
	resp, err := c.do(ctx, c.stream, method, path, body, "application/json")
	if err != nil {
		return api.Event{}, err
	}
	defer resp.Body.Close()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-ndjson") {
		_, _ = io.Copy(io.Discard, resp.Body)
		return api.Event{Status: "result"}, nil
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var last api.Event
	for sc.Scan() {
		var e api.Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Status {
		case "result":
			return e, nil
		case "error":
			return e, errors.New(e.Message)
		}
		last = e
		onEvent(e)
	}
	if err := sc.Err(); err != nil {
		return last, err
	}
	return last, errors.New("connection closed before the operation finished — it keeps running on the server; check `dokwalt releases`")
}

// Logs streams log lines until ctx is cancelled or the stream ends.
func (c *Client) Logs(ctx context.Context, path string, q url.Values, fn func(api.LogLine)) error {
	resp, err := c.do(ctx, c.stream, "GET", path+"?"+q.Encode(), nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	go func() { <-ctx.Done(); resp.Body.Close() }()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var l api.LogLine
		if json.Unmarshal(sc.Bytes(), &l) == nil {
			fn(l)
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return sc.Err()
}

// Upload streams a request body (used for image transfer).
func (c *Client) Upload(ctx context.Context, path string, body io.Reader) error {
	resp, err := c.do(ctx, c.stream, "POST", path, body, "application/octet-stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// Tunnel opens a TCP connection to addr as seen from the server.
func (c *Client) Tunnel(addr string) (net.Conn, error) {
	if c.SSH == nil {
		return net.Dial("tcp", addr)
	}
	return c.SSH.Exec(c.RemoteBin + " dial-stdio --tcp " + sshx.ShellQuote(addr))
}

// StagePath builds /v1/apps/{app}/stages/{stage}{suffix}.
func StagePath(app, stage, suffix string) string {
	return "/v1/apps/" + url.PathEscape(app) + "/stages/" + url.PathEscape(stage) + suffix
}
