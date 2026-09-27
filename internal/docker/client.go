// Package docker is a deliberately tiny Docker Engine API client over the
// Unix socket. It covers only what DokWalt needs, which keeps the binary and
// the daemon's memory footprint small compared with the official SDK.
package docker

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	socket  string
	http    *http.Client
	stream  *http.Client // no timeout, for logs/events/load
	version string       // negotiated API version, e.g. "1.47"
}

func New(socket string) *Client {
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}
	return &Client{
		socket: socket,
		http:   &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{DialContext: dial, MaxIdleConns: 4}},
		stream: &http.Client{Transport: &http.Transport{DialContext: dial, DisableKeepAlives: true}},
	}
}

// Negotiate picks the daemon's API version. Call once at startup.
func (c *Client) Negotiate(ctx context.Context) (Version, error) {
	var v Version
	if err := c.do(ctx, c.http, "GET", "/version", nil, nil, &v); err != nil {
		return v, err
	}
	c.version = v.APIVersion
	return v, nil
}

func (c *Client) path(p string) string {
	if c.version == "" {
		return p
	}
	return "/v" + c.version + p
}

type apiError struct {
	Status  int
	Message string `json:"message"`
}

func (e *apiError) Error() string { return fmt.Sprintf("docker: %s (HTTP %d)", e.Message, e.Status) }

// IsNotFound reports whether err is a Docker 404.
func IsNotFound(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

func (c *Client) raw(ctx context.Context, hc *http.Client, method, p string, q url.Values, body io.Reader, ctype string) (*http.Response, error) {
	u := "http://docker" + c.path(p)
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		ae := &apiError{Status: resp.StatusCode}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(b, ae) != nil || ae.Message == "" {
			ae.Message = strings.TrimSpace(string(b))
		}
		return nil, ae
	}
	return resp, nil
}

func (c *Client) do(ctx context.Context, hc *http.Client, method, p string, q url.Values, in, out any) error {
	var body io.Reader
	ctype := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(b))
		ctype = "application/json"
	}
	resp, err := c.raw(ctx, hc, method, p, q, body, ctype)
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

// ---- types (only the fields we use) ----

type Version struct {
	Version    string `json:"Version"`
	APIVersion string `json:"ApiVersion"`
	Os         string `json:"Os"`
	Arch       string `json:"Arch"`
}

type Info struct {
	CgroupDriver  string `json:"CgroupDriver"`
	CgroupVersion string `json:"CgroupVersion"`
	MemoryLimit   bool   `json:"MemoryLimit"`
	NCPU          int    `json:"NCPU"`
	MemTotal      int64  `json:"MemTotal"`
	DockerRootDir string `json:"DockerRootDir"`
	LiveRestore   bool   `json:"LiveRestoreEnabled"`
	Architecture  string `json:"Architecture"`
	Warnings      []string
}

type ContainerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
}

func (s ContainerSummary) Name() string {
	if len(s.Names) == 0 {
		return s.ID[:12]
	}
	return strings.TrimPrefix(s.Names[0], "/")
}

type ContainerJSON struct {
	ID           string `json:"Id"`
	Name         string `json:"Name"`
	RestartCount int    `json:"RestartCount"`
	Image        string `json:"Image"`
	State        struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		Restarting bool   `json:"Restarting"`
		OOMKilled  bool   `json:"OOMKilled"`
		Pid        int    `json:"Pid"`
		ExitCode   int    `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		Health     *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Env    []string          `json:"Env"`
		Labels map[string]string `json:"Labels"`
		Tty    bool              `json:"Tty"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string   `json:"IPAddress"`
			Aliases   []string `json:"Aliases"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
	HostConfig struct {
		Memory int64 `json:"Memory"`
	} `json:"HostConfig"`
}

func (c ContainerJSON) Health() string {
	if c.State.Health == nil {
		return ""
	}
	return c.State.Health.Status
}

// Env returns the container environment as a map.
func (c ContainerJSON) EnvMap() map[string]string {
	m := map[string]string{}
	for _, kv := range c.Config.Env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

type ImageSummary struct {
	ID       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
	Created  int64    `json:"Created"`
	Size     int64    `json:"Size"`
}

type Event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	Time int64 `json:"time"`
}

// ---- calls ----

func (c *Client) Info(ctx context.Context) (Info, error) {
	var i Info
	err := c.do(ctx, c.http, "GET", "/info", nil, nil, &i)
	return i, err
}

func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.raw(ctx, c.http, "GET", "/_ping", nil, nil, "")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func labelFilters(labels ...string) url.Values {
	f := map[string][]string{"label": labels}
	b, _ := json.Marshal(f)
	return url.Values{"filters": {string(b)}}
}

// Containers lists containers (including stopped ones) matching all labels ("k=v" or "k").
func (c *Client) Containers(ctx context.Context, labels ...string) ([]ContainerSummary, error) {
	q := labelFilters(labels...)
	q.Set("all", "1")
	var out []ContainerSummary
	err := c.do(ctx, c.http, "GET", "/containers/json", q, nil, &out)
	return out, err
}

func (c *Client) Inspect(ctx context.Context, id string) (ContainerJSON, error) {
	var out ContainerJSON
	err := c.do(ctx, c.http, "GET", "/containers/"+id+"/json", nil, nil, &out)
	return out, err
}

func (c *Client) Restart(ctx context.Context, id string) error {
	return c.do(ctx, c.http, "POST", "/containers/"+id+"/restart", url.Values{"t": {"10"}}, nil, nil)
}

// ImageExists reports whether an image (ID or ref) is present.
func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	err := c.do(ctx, c.http, "GET", "/images/"+ref+"/json", nil, nil, nil)
	if IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (c *Client) ImageID(ctx context.Context, ref string) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	err := c.do(ctx, c.http, "GET", "/images/"+ref+"/json", nil, nil, &out)
	return out.ID, err
}

func (c *Client) Images(ctx context.Context, reference string) ([]ImageSummary, error) {
	q := url.Values{}
	if reference != "" {
		b, _ := json.Marshal(map[string][]string{"reference": {reference}})
		q.Set("filters", string(b))
	}
	var out []ImageSummary
	err := c.do(ctx, c.http, "GET", "/images/json", q, nil, &out)
	return out, err
}

func (c *Client) RemoveImage(ctx context.Context, ref string) error {
	return c.do(ctx, c.http, "DELETE", "/images/"+ref, nil, nil, nil)
}

// LoadImage streams a `docker save` tarball into the daemon.
func (c *Client) LoadImage(ctx context.Context, tar io.Reader) error {
	resp, err := c.raw(ctx, c.stream, "POST", "/images/load", url.Values{"quiet": {"1"}}, tar, "application/x-tar")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// The response is a JSON message stream; surface errors.
	dec := json.NewDecoder(resp.Body)
	for {
		var m struct {
			Error  string `json:"error"`
			Stream string `json:"stream"`
		}
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if m.Error != "" {
			return errors.New(m.Error)
		}
	}
}

func (c *Client) NetworkExists(ctx context.Context, name string) (bool, error) {
	err := c.do(ctx, c.http, "GET", "/networks/"+name, nil, nil, nil)
	if IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (c *Client) CreateNetwork(ctx context.Context, name string, labels map[string]string) error {
	body := map[string]any{"Name": name, "Driver": "bridge", "Labels": labels, "CheckDuplicate": true}
	return c.do(ctx, c.http, "POST", "/networks/create", nil, body, nil)
}

func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	err := c.do(ctx, c.http, "DELETE", "/networks/"+name, nil, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

func (c *Client) EnsureNetwork(ctx context.Context, name string, labels map[string]string) error {
	ok, err := c.NetworkExists(ctx, name)
	if err != nil || ok {
		return err
	}
	return c.CreateNetwork(ctx, name, labels)
}

// ConnectNetwork attaches a container to a network; already-connected is not an error.
func (c *Client) ConnectNetwork(ctx context.Context, network, container string) error {
	err := c.do(ctx, c.http, "POST", "/networks/"+network+"/connect", nil, map[string]any{"Container": container}, nil)
	var ae *apiError
	if errors.As(err, &ae) && (ae.Status == http.StatusForbidden || strings.Contains(ae.Message, "already exists")) {
		return nil
	}
	return err
}

func (c *Client) DisconnectNetwork(ctx context.Context, network, container string) error {
	err := c.do(ctx, c.http, "POST", "/networks/"+network+"/disconnect", nil, map[string]any{"Container": container, "Force": true}, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// NetworkContainers returns the names of containers attached to a network.
func (c *Client) NetworkContainers(ctx context.Context, network string) (map[string]bool, error) {
	var out struct {
		Containers map[string]struct {
			Name string `json:"Name"`
		} `json:"Containers"`
	}
	if err := c.do(ctx, c.http, "GET", "/networks/"+network, nil, nil, &out); err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, v := range out.Containers {
		names[v.Name] = true
	}
	return names, nil
}

func (c *Client) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	return c.do(ctx, c.http, "POST", "/volumes/create", nil, map[string]any{"Name": name, "Labels": labels}, nil)
}

// Volumes lists volume names having the label.
func (c *Client) Volumes(ctx context.Context, label string) ([]string, error) {
	var out struct {
		Volumes []struct {
			Name string `json:"Name"`
		} `json:"Volumes"`
	}
	if err := c.do(ctx, c.http, "GET", "/volumes", labelFilters(label), nil, &out); err != nil {
		return nil, err
	}
	var names []string
	for _, v := range out.Volumes {
		names = append(names, v.Name)
	}
	return names, nil
}

func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	err := c.do(ctx, c.http, "DELETE", "/volumes/"+name, nil, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

type Stats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemUsage uint64 `json:"system_cpu_usage"`
		OnlineCPUs  int    `json:"online_cpus"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
	BlkioStats struct {
		IoServiceBytesRecursive []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
}

// StatsOnce returns a single stats sample without waiting for a second one.
func (c *Client) StatsOnce(ctx context.Context, id string) (Stats, error) {
	var s Stats
	err := c.do(ctx, c.http, "GET", "/containers/"+id+"/stats", url.Values{"stream": {"0"}, "one-shot": {"1"}}, nil, &s)
	return s, err
}

// Events streams Docker events until ctx is cancelled.
func (c *Client) Events(ctx context.Context, filters map[string][]string, fn func(Event)) error {
	b, _ := json.Marshal(filters)
	resp, err := c.raw(ctx, c.stream, "GET", "/events", url.Values{"filters": {string(b)}}, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var e Event
		if err := dec.Decode(&e); err != nil {
			return err
		}
		fn(e)
	}
}

// LogOptions for Logs.
type LogOptions struct {
	Follow bool
	Tail   string // "all" or number
	Since  int64  // unix seconds
}

// Logs streams a container's logs, calling fn per line. The stream is
// demultiplexed unless the container has a TTY.
func (c *Client) Logs(ctx context.Context, id string, tty bool, opt LogOptions, fn func(stream string, t time.Time, line string)) error {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "timestamps": {"1"}}
	if opt.Follow {
		q.Set("follow", "1")
	}
	if opt.Tail != "" {
		q.Set("tail", opt.Tail)
	}
	if opt.Since > 0 {
		q.Set("since", fmt.Sprint(opt.Since))
	}
	resp, err := c.raw(ctx, c.stream, "GET", "/containers/"+id+"/logs", q, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	emit := func(stream, raw string) {
		ts, line, ok := strings.Cut(raw, " ")
		t, err := time.Parse(time.RFC3339Nano, ts)
		if !ok || err != nil {
			t, line = time.Now(), raw
		}
		fn(stream, t, strings.TrimRight(line, "\r\n"))
	}
	if tty {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			emit("stdout", sc.Text())
		}
		return sc.Err()
	}
	// Multiplexed: 8-byte header [stream,0,0,0,size(4)] then payload.
	r := bufio.NewReader(resp.Body)
	hdr := make([]byte, 8)
	partial := map[byte]string{}
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		n := binary.BigEndian.Uint32(hdr[4:])
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return err
		}
		stream := "stdout"
		if hdr[0] == 2 {
			stream = "stderr"
		}
		data := partial[hdr[0]] + string(buf)
		for {
			i := strings.IndexByte(data, '\n')
			if i < 0 {
				break
			}
			emit(stream, data[:i])
			data = data[i+1:]
		}
		partial[hdr[0]] = data
	}
}
