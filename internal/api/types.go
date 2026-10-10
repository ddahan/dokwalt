// Package api holds the request/response types shared by the CLI and the daemon.
package api

import "time"

// Version is the API version. The CLI refuses to talk to a daemon with a
// different major version and warns on a minor mismatch.
const Version = "1.0"

// DefaultStage is the only stage of an app without a pipeline.
const DefaultStage = "production"

// StagingStage is added by `pipeline:enable`.
const StagingStage = "staging"

type VersionInfo struct {
	APIVersion string `json:"api_version"`
	Build      string `json:"build"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
}

type Error struct {
	Error string `json:"error"`
	Hint  string `json:"hint,omitempty"`
}

type ServerInfo struct {
	Version       VersionInfo `json:"version"`
	Hostname      string      `json:"hostname"`
	Uptime        int64       `json:"uptime_s"`
	DockerVersion string      `json:"docker_version"`
	ComposeVer    string      `json:"compose_version"`
	DaemonRSS     uint64      `json:"daemon_rss"`
	DaemonHeap    uint64      `json:"daemon_heap"`
	CaddyRSS      uint64      `json:"caddy_rss"`
	CaddyHeap     uint64      `json:"caddy_heap"`
	Apps          int         `json:"apps"`
	Host          HostMetrics `json:"host"`
	ACMEEmail     string      `json:"acme_email"`
}

type App struct {
	Name      string    `json:"name"`
	Pipeline  bool      `json:"pipeline"`
	CreatedAt time.Time `json:"created_at"`
	Stages    []Stage   `json:"stages"`
}

type Stage struct {
	Name           string   `json:"name"`
	ActiveColor    string   `json:"active_color,omitempty"`
	CurrentRelease int      `json:"current_release,omitempty"`
	Status         string   `json:"status"` // running | degraded | stopped | not deployed | deploying
	Domains        []string `json:"domains,omitempty"`
}

type Release struct {
	Version       int               `json:"version"`
	Status        string            `json:"status"`
	Images        map[string]Image  `json:"images"`
	ConfigVersion int               `json:"config_version"`
	Color         string            `json:"color"`
	Description   string            `json:"description"`
	GitSHA        string            `json:"git_sha,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
	FinishedAt    *time.Time        `json:"finished_at,omitempty"`
	Error         string            `json:"error,omitempty"`
	Current       bool              `json:"current"`
	Extra         map[string]string `json:"extra,omitempty"`
}

type Image struct {
	Ref string `json:"ref"`
	ID  string `json:"id,omitempty"`
}

// Service is a compose service as DokWalt sees it.
type Service struct {
	Name       string       `json:"name"`
	Image      string       `json:"image"`
	Stateful   bool         `json:"stateful"`
	Detected   string       `json:"detected"` // why it's (not) stateful
	Overridden bool         `json:"overridden"`
	HealthPath string       `json:"health_path,omitempty"`
	Containers []Container  `json:"containers,omitempty"`
	Metrics    *LiveMetrics `json:"metrics,omitempty"`
}

type Container struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Health   string `json:"health,omitempty"`
	Status   string `json:"status"`
	Restarts int    `json:"restarts"`
	Color    string `json:"color,omitempty"`
	IP       string `json:"ip,omitempty"`
}

type Domain struct {
	Hostname   string `json:"hostname"`
	Stage      string `json:"stage"`
	Service    string `json:"service,omitempty"`
	Port       int    `json:"port,omitempty"`
	RedirectTo string `json:"redirect_to,omitempty"`
	Check      string `json:"check,omitempty"` // ok | unreachable: ... | unknown
}

type DomainAddRequest struct {
	Hostname   string `json:"hostname"`
	Service    string `json:"service,omitempty"`
	Port       int    `json:"port,omitempty"`
	RedirectTo string `json:"redirect_to,omitempty"`
}

type ConfigVar struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type ConfigSetRequest struct {
	Set   map[string]string `json:"set,omitempty"`
	Unset []string          `json:"unset,omitempty"`
	// NoRestart stores the change without creating a release.
	NoRestart bool `json:"no_restart,omitempty"`
}

type ServiceOverride struct {
	Service       string `json:"service"`
	Stateful      *bool  `json:"stateful,omitempty"`
	HealthPath    string `json:"health_path,omitempty"`
	HealthTimeout int    `json:"health_timeout_s,omitempty"`
	ClearHealth   bool   `json:"clear_health,omitempty"`
}

// DeployRequest is sent after images have been transferred.
type DeployRequest struct {
	Compose     map[string]any   `json:"compose"` // `docker compose config --no-interpolate --format json`
	Images      map[string]Image `json:"images"`  // built service -> image
	Description string           `json:"description"`
	GitSHA      string           `json:"git_sha,omitempty"`
}

type RollbackRequest struct {
	Version    int  `json:"version"` // 0 = previous succeeded release
	WithConfig bool `json:"with_config"`
}

// Event is one line of a streamed NDJSON operation (deploy, promote, ...).
type Event struct {
	Time    time.Time `json:"time"`
	Step    string    `json:"step,omitempty"` // stable identifier of the step
	Status  string    `json:"status"`         // start | progress | done | warn | error | log | result
	Message string    `json:"message"`
	Service string    `json:"service,omitempty"`
	Release int       `json:"release,omitempty"`
}

type LogLine struct {
	Time    time.Time `json:"time"`
	Service string    `json:"service"`
	Replica int       `json:"replica,omitempty"`
	Stream  string    `json:"stream"`
	Line    string    `json:"line"`
}

type ImagesHaveRequest struct {
	IDs []string `json:"ids"`
}

type ImagesHaveResponse struct {
	Have map[string]bool `json:"have"`
}

type LiveMetrics struct {
	CPUPct   float64 `json:"cpu_pct"`
	MemBytes uint64  `json:"mem_bytes"`
	MemLimit uint64  `json:"mem_limit,omitempty"`
	NetRx    uint64  `json:"net_rx_bps"`
	NetTx    uint64  `json:"net_tx_bps"`
	BlkR     uint64  `json:"blk_r_bps"`
	BlkW     uint64  `json:"blk_w_bps"`
}

type HostMetrics struct {
	CPUPct    float64 `json:"cpu_pct"`
	CPUs      int     `json:"cpus"`
	MemUsed   uint64  `json:"mem_used"`
	MemTotal  uint64  `json:"mem_total"`
	DiskUsed  uint64  `json:"disk_used"`
	DiskTotal uint64  `json:"disk_total"`
	Load1     float64 `json:"load1"`
	TempC     float64 `json:"temp_c,omitempty"`
	Throttled string  `json:"throttled,omitempty"`
}

type HTTPMetrics struct {
	Hostname string  `json:"hostname"`
	RPS      float64 `json:"rps"`
	Requests uint64  `json:"requests"`
	S2xx     uint64  `json:"s2xx"`
	S3xx     uint64  `json:"s3xx"`
	S4xx     uint64  `json:"s4xx"`
	S5xx     uint64  `json:"s5xx"`
	P50      float64 `json:"p50_ms"`
	P95      float64 `json:"p95_ms"`
	P99      float64 `json:"p99_ms"`
}

// TopSnapshot is the live view used by `dokwalt top` and the dashboard.
type TopSnapshot struct {
	Time     time.Time            `json:"time"`
	Host     HostMetrics          `json:"host"`
	Services []TopService         `json:"services"`
	HTTP     []HTTPMetrics        `json:"http"`
	History  map[string][]float64 `json:"history,omitempty"` // "app/stage" -> recent CPU%, oldest first
}

type TopService struct {
	App      string      `json:"app"`
	Stage    string      `json:"stage"`
	Service  string      `json:"service"`
	Replicas int         `json:"replicas"`
	Metrics  LiveMetrics `json:"metrics"`
}

type MetricsPoint struct {
	TS       int64   `json:"ts"`
	CPUPct   float64 `json:"cpu_pct"`
	MemBytes uint64  `json:"mem_bytes"`
	Requests uint64  `json:"requests,omitempty"`
	S5xx     uint64  `json:"s5xx,omitempty"`
	P95      float64 `json:"p95_ms,omitempty"`
}

type MetricsHistory struct {
	App      string                    `json:"app,omitempty"`
	Services map[string][]MetricsPoint `json:"services"`
	HTTP     map[string][]MetricsPoint `json:"http"`
	Host     []MetricsPoint            `json:"host"`
}

type AlertChannel struct {
	ID   int    `json:"id"`
	Kind string `json:"kind"`
	URL  string `json:"url"`
}

type AlertsConfig struct {
	Channels   []AlertChannel    `json:"channels"`
	Thresholds map[string]string `json:"thresholds"`
	Firing     []string          `json:"firing"`
}

type DoctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // ok | warn | fail
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// DBTarget tells the CLI where to tunnel for db:connect, and where to run
// pg_dump for db:backup.
type DBTarget struct {
	Service   string `json:"service"`
	Kind      string `json:"kind"`    // postgres | mysql | mongo | redis | unknown
	Address   string `json:"address"` // ip:port reachable from the server
	User      string `json:"user,omitempty"`
	Password  string `json:"password,omitempty"`
	Database  string `json:"database,omitempty"`
	Container string `json:"container,omitempty"` // container running the database
	Provider  string `json:"provider,omitempty"`  // owning stage when shared (x-dokwalt.uses), e.g. "postgres/production"
}

type ExecTarget struct {
	Container string `json:"container"`
}

// AppExport is the portable server-side state of an app (apps:export/import).
type AppExport struct {
	Format   int                    `json:"format"`
	Name     string                 `json:"name"`
	Pipeline bool                   `json:"pipeline"`
	Stages   map[string]StageExport `json:"stages"`
}

type StageExport struct {
	Config    map[string]string `json:"config"`
	Domains   []Domain          `json:"domains"`
	Overrides []ServiceOverride `json:"overrides"`
}
