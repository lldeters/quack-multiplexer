// Package config defines the gateway configuration file (YAML) and its
// defaults and validation rules.
package config

import (
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

// Config is the root of the configuration file.
type Config struct {
	Server   ServerConfig   `json:"server"`
	Admin    AdminConfig    `json:"admin"`
	Auth     AuthConfig     `json:"auth"`
	Defaults SessionOptions `json:"defaults"`
	// Overrides adjust defaults and permissions for matching users/groups.
	// The first matching entry wins.
	Overrides []Override         `json:"overrides,omitempty"`
	Session   SessionConfig      `json:"session"`
	Engine    EngineConfig       `json:"engine"`
	Limits    LimitsConfig       `json:"limits"`
	Provider  ProviderConfig     `json:"provider"`
	Profiles  map[string]Profile `json:"profiles"`
	Audit     AuditConfig        `json:"audit"`
	HA        HAConfig           `json:"ha"`
	Log       LogConfig          `json:"log"`
}

// ServerConfig configures the Quack-facing listener.
type ServerConfig struct {
	// Listen is the address of the Quack endpoint (default ":9494").
	Listen string     `json:"listen"`
	TLS    *TLSConfig `json:"tls,omitempty"`
	// SessionKeyFile holds the secret that seals connection ids. All replicas
	// must share it. The QUACKMUX_SESSION_KEY environment variable wins over
	// the file. Without either, a random key is generated and sessions do not
	// survive a gateway restart.
	SessionKeyFile string `json:"sessionKeyFile,omitempty"`
	// Banner is returned on GET /.
	Banner string `json:"banner,omitempty"`
	// AllowedOrigins for CORS (DuckDB-Wasm clients). Default ["*"] like the
	// stock Quack server, but none with auth.mode none: without
	// authentication, any web page could run queries through the browser of
	// a user who reaches the gateway. Requests that carry an Origin header
	// (browsers send it with every POST) from other origins are refused. An
	// explicit empty list allows none.
	AllowedOrigins []string `json:"allowedOrigins,omitempty"`
	// MaxHandshakeBytes bounds a CONNECTION_REQUEST body (default 64KiB).
	MaxHandshakeBytes int `json:"maxHandshakeBytes,omitempty"`
	// ReadHeaderTimeout for incoming HTTP requests (default 30s).
	ReadHeaderTimeout Duration `json:"readHeaderTimeout,omitempty"`
	// IdleTimeout for keep-alive client connections (default 300s, the stock
	// quack_server_keep_alive_timeout).
	IdleTimeout Duration `json:"idleTimeout,omitempty"`
	// ShutdownTimeout bounds the graceful drain of in-flight requests.
	ShutdownTimeout Duration `json:"shutdownTimeout,omitempty"`
	// Upstream configures the gateway's HTTP client towards engines.
	Upstream UpstreamConfig `json:"upstream"`
}

// TLSConfig enables HTTPS on a listener.
type TLSConfig struct {
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
}

// UpstreamConfig tunes the connections from the gateway to engines.
type UpstreamConfig struct {
	DialTimeout Duration `json:"dialTimeout,omitempty"`
	// ResponseHeaderTimeout 0 disables the limit. FETCH requests block on the
	// engine until a batch is ready, so a limit must exceed the longest
	// expected wait for the first rows of a query.
	ResponseHeaderTimeout Duration `json:"responseHeaderTimeout,omitempty"`
	MaxIdleConnsPerEngine int      `json:"maxIdleConnsPerEngine,omitempty"`
	IdleConnTimeout       Duration `json:"idleConnTimeout,omitempty"`
	// ControlTimeout bounds requests the gateway sends on its own
	// (DISCONNECT, CANCEL on behalf of a closed session).
	ControlTimeout Duration `json:"controlTimeout,omitempty"`
	// HandshakeTimeout bounds the handshake with an engine (the
	// CONNECTION_REQUEST of a client), which a healthy engine answers at
	// once. It is independent of the client: a client that gives up does
	// not leave a half-created backend connection behind.
	HandshakeTimeout Duration `json:"handshakeTimeout,omitempty"`
	// The engine's HTTP server (cpp-httplib) aborts a connection when the
	// peer stalls for 5s and rejects bodies over 100 MiB. Like nginx, the
	// gateway therefore buffers messages in memory so that slow clients never
	// stall an engine. Messages up to MaxBufferedRequestBytes /
	// MaxBufferedResponseBytes are buffered while the total stays within
	// BufferBudgetBytes; anything else is streamed. A negative value disables
	// buffering in that direction (always stream).
	MaxBufferedRequestBytes  int64 `json:"maxBufferedRequestBytes,omitempty"`
	MaxBufferedResponseBytes int64 `json:"maxBufferedResponseBytes,omitempty"`
	BufferBudgetBytes        int64 `json:"bufferBudgetBytes,omitempty"`
	// MaxRequestBytes rejects larger messages up front (default 100 MiB, the
	// engine's own limit). 0 disables the check.
	MaxRequestBytes int64 `json:"maxRequestBytes,omitempty"`
}

// AdminConfig configures the admin/metrics listener.
type AdminConfig struct {
	// Listen address; empty disables the admin listener.
	Listen string `json:"listen"`
	// TokenFile holds the bearer token for /api. Without it the API is open
	// (only acceptable on a private network); /metrics, /healthz and /readyz
	// never need a token.
	TokenFile string     `json:"tokenFile,omitempty"`
	TLS       *TLSConfig `json:"tls,omitempty"`
}

// AuthConfig selects and configures the authenticator.
type AuthConfig struct {
	// Mode is one of "none", "static", "jwt".
	Mode   string           `json:"mode"`
	None   NoneAuthConfig   `json:"none"`
	Static StaticAuthConfig `json:"static"`
	JWT    JWTAuthConfig    `json:"jwt"`
}

// NoneAuthConfig is for development only: every token is accepted.
type NoneAuthConfig struct {
	// DefaultUser is used when the client names no user (default "anonymous").
	DefaultUser string `json:"defaultUser,omitempty"`
	// DefaultGroups are assigned to every identity.
	DefaultGroups []string `json:"defaultGroups,omitempty"`
}

// StaticAuthConfig reads users and token hashes from a YAML file.
type StaticAuthConfig struct {
	File string `json:"file"`
	// ReloadInterval re-reads the file when it changed (default 10s, 0 disables).
	ReloadInterval Duration `json:"reloadInterval,omitempty"`
}

// JWTAuthConfig validates JSON Web Tokens.
type JWTAuthConfig struct {
	Issuer   string   `json:"issuer,omitempty"`
	Audience []string `json:"audience,omitempty"`
	// UserClaim names the user (default "sub").
	UserClaim string `json:"userClaim,omitempty"`
	// GroupsClaim names a string or string-array claim (default "groups").
	GroupsClaim string `json:"groupsClaim,omitempty"`
	// Algorithms permitted (default ["RS256","ES256","HS256"] restricted to
	// the configured key types).
	Algorithms []string `json:"algorithms,omitempty"`
	// HMACSecretFile holds a shared secret for HS256/HS384/HS512.
	HMACSecretFile string `json:"hmacSecretFile,omitempty"`
	// PublicKeyFiles hold PEM encoded RSA/ECDSA/Ed25519 public keys.
	PublicKeyFiles []string `json:"publicKeyFiles,omitempty"`
	// JWKSURL is fetched and refreshed periodically.
	JWKSURL     string   `json:"jwksURL,omitempty"`
	JWKSRefresh Duration `json:"jwksRefresh,omitempty"`
	// Leeway tolerates clock skew for exp/nbf/iat (default 30s).
	Leeway Duration `json:"leeway,omitempty"`
}

// SessionOptions are the per-session routing options a client may request.
type SessionOptions struct {
	ShareLevel string `json:"shareLevel,omitempty"`
	Subdomain  string `json:"subdomain,omitempty"`
	Profile    string `json:"profile,omitempty"`
}

// Override adjusts defaults and permissions for some users or groups.
type Override struct {
	Users    []string       `json:"users,omitempty"`
	Groups   []string       `json:"groups,omitempty"`
	Defaults SessionOptions `json:"defaults,omitempty"`
	// AllowedShareLevels / AllowedProfiles restrict what matching users may
	// request (empty = no additional restriction).
	AllowedShareLevels []string `json:"allowedShareLevels,omitempty"`
	AllowedProfiles    []string `json:"allowedProfiles,omitempty"`
	// MaxSessions overrides limits.maxSessionsPerUser for matching users.
	MaxSessions int `json:"maxSessions,omitempty"`
}

// Matches reports whether the override applies to the identity.
func (o Override) Matches(user string, groups []string) bool {
	for _, u := range o.Users {
		if u == user || u == "*" {
			return true
		}
	}
	for _, g := range o.Groups {
		for _, have := range groups {
			if g == have {
				return true
			}
		}
	}
	return false
}

// SessionConfig controls session lifecycle.
type SessionConfig struct {
	// IdleTimeout closes sessions without queries (heartbeats do not count)
	// for this long. 0 disables. Default 6h (Kyuubi: kyuubi.session.idle.timeout).
	IdleTimeout Duration `json:"idleTimeout"`
	// MaxLifetime closes sessions older than this. 0 disables.
	MaxLifetime Duration `json:"maxLifetime"`
	// MinHeartbeatTimeout / MaxHeartbeatTimeout clamp the heartbeat lease the
	// client asks for (DuckDB default: 60s).
	MinHeartbeatTimeout Duration `json:"minHeartbeatTimeout"`
	MaxHeartbeatTimeout Duration `json:"maxHeartbeatTimeout"`
	// HeartbeatGrace is added to the lease before the gateway declares a
	// silent client dead and frees its engine connection.
	HeartbeatGrace Duration `json:"heartbeatGrace"`
	// BackendHeartbeatTimeout is the lease the gateway requests from the
	// engine. The gateway answers client heartbeats itself (a hung engine can
	// then never block a client's DETACH) and renews the engine lease every
	// third of this duration for quiet sessions. Default 1h.
	BackendHeartbeatTimeout Duration `json:"backendHeartbeatTimeout"`
	// TombstoneTTL keeps closed sessions around so clients get a precise
	// error message instead of "Invalid connection id".
	TombstoneTTL Duration `json:"tombstoneTTL"`
	// CheckInterval is the reaper period.
	CheckInterval Duration `json:"checkInterval"`
}

// EngineConfig controls engine lifecycle. Profiles may override timeouts.
type EngineConfig struct {
	// StartupTimeout bounds launching an engine until it is ready
	// (Kyuubi: kyuubi.session.engine.initialize.timeout).
	StartupTimeout Duration `json:"startupTimeout"`
	// IdleTimeout per share level: an engine without sessions for this long is
	// terminated (Kyuubi: kyuubi.session.engine.idle.timeout). CONNECTION
	// engines default to 0 = terminate as soon as their session ends.
	IdleTimeout map[string]Duration `json:"idleTimeout"`
	// MaxLifetime drains engines older than this (0 disables).
	MaxLifetime Duration `json:"maxLifetime"`
	// DrainTimeout force-closes sessions of a draining engine after this long.
	DrainTimeout Duration `json:"drainTimeout"`
	// RecoveryGrace keeps recovered engines alive after a gateway (re)start so
	// that their sessions can re-attach with their next heartbeat.
	RecoveryGrace Duration `json:"recoveryGrace"`
	// CheckInterval is the reconcile period.
	CheckInterval Duration `json:"checkInterval"`
	// ReplaceOnSpecChange drains engines whose profile changed since launch.
	ReplaceOnSpecChange *bool `json:"replaceOnSpecChange,omitempty"`
	// FailedRetention keeps failed engines (pods) this long for debugging
	// before deleting them.
	FailedRetention Duration `json:"failedRetention"`
}

// LimitsConfig protects the cluster. 0 means unlimited.
type LimitsConfig struct {
	MaxSessions        int `json:"maxSessions"`
	MaxSessionsPerUser int `json:"maxSessionsPerUser"`
	MaxEngines         int `json:"maxEngines"`
	MaxEnginesPerUser  int `json:"maxEnginesPerUser"`
	// MaxStartingEngines bounds concurrent launches.
	MaxStartingEngines int `json:"maxStartingEngines"`
}

// ProviderConfig selects where engines run.
type ProviderConfig struct {
	// Type is "kubernetes", "process" or "static".
	Type       string                   `json:"type"`
	Kubernetes KubernetesProviderConfig `json:"kubernetes"`
	Process    ProcessProviderConfig    `json:"process"`
	Static     StaticProviderConfig     `json:"static"`
}

// KubernetesProviderConfig configures engine pods.
type KubernetesProviderConfig struct {
	// Namespace for engine pods (default: the gateway's own namespace).
	Namespace string `json:"namespace,omitempty"`
	// Kubeconfig path; empty means in-cluster configuration.
	Kubeconfig string `json:"kubeconfig,omitempty"`
	// Instance separates several gateways in one namespace (label value).
	Instance string `json:"instance,omitempty"`
	// NamePrefix for engine pods (default "quack").
	NamePrefix string `json:"namePrefix,omitempty"`
	// DeleteGracePeriod for engine pods (default 30s).
	DeleteGracePeriod Duration `json:"deleteGracePeriod,omitempty"`
	// QPS/Burst for the API client.
	QPS   float32 `json:"qps,omitempty"`
	Burst int     `json:"burst,omitempty"`
}

// ProcessProviderConfig runs engines as local processes (development).
type ProcessProviderConfig struct {
	// DuckDBPath is the DuckDB CLI binary (default "duckdb" from PATH).
	DuckDBPath string `json:"duckdbPath,omitempty"`
	// WorkDir holds per-engine scratch directories.
	WorkDir string `json:"workDir,omitempty"`
	// BindHost for engines (default 127.0.0.1).
	BindHost string `json:"bindHost,omitempty"`
	// PortRangeStart/End for engines (default 19500-19999).
	PortRangeStart int `json:"portRangeStart,omitempty"`
	PortRangeEnd   int `json:"portRangeEnd,omitempty"`
}

// StaticProviderConfig routes to engines that are managed elsewhere.
type StaticProviderConfig struct {
	Engines []StaticEngine `json:"engines"`
	// HealthInterval for GET / probes (default 10s).
	HealthInterval Duration `json:"healthInterval,omitempty"`
	// FailureThreshold is the number of probes in a row that must fail
	// before an engine is taken out of rotation (default 3). Its sessions
	// are closed then, so a single lost probe (a busy engine that sheds a
	// connection) must not count.
	FailureThreshold int `json:"failureThreshold,omitempty"`
}

// StaticEngine is a pre-existing Quack server.
type StaticEngine struct {
	Name string `json:"name"`
	// URL like http://duckdb.example:9494 (without /quack).
	URL       string `json:"url"`
	Token     string `json:"token,omitempty"`
	TokenFile string `json:"tokenFile,omitempty"`
	// The engine key this server answers for.
	ShareLevel string `json:"shareLevel"`
	Owner      string `json:"owner,omitempty"`
	Subdomain  string `json:"subdomain,omitempty"`
	Profile    string `json:"profile,omitempty"`
	PoolIndex  int    `json:"poolIndex,omitempty"`
}

// Profile describes one kind of engine.
type Profile struct {
	Description string `json:"description,omitempty"`
	// Image is the engine container image (DuckDB 2.0 with quack). It
	// replaces the image of the podTemplate's engine container; without
	// both, DefaultEngineImage applies.
	Image string `json:"image,omitempty"`
	// ImagePullPolicy likewise replaces the podTemplate's (default
	// IfNotPresent).
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
	// Database is the DuckDB database path inside the engine (":memory:";
	// with Persistence <mountPath>/engine.duckdb).
	Database string `json:"database,omitempty"`
	// InitSQL runs before the Quack server starts (ATTACH, SET, secrets...).
	InitSQL string `json:"initSQL,omitempty"`
	// Settings are applied with SET before the server starts.
	Settings map[string]string `json:"settings,omitempty"`
	// Pool spreads sessions of one key over several engines.
	Pool PoolConfig `json:"pool,omitempty"`
	// WarmPool keeps idle engines ready for CONNECTION share level.
	WarmPool WarmPoolConfig `json:"warmPool,omitempty"`
	// Access control. Empty means unrestricted.
	AllowedShareLevels []string `json:"allowedShareLevels,omitempty"`
	AllowedUsers       []string `json:"allowedUsers,omitempty"`
	AllowedGroups      []string `json:"allowedGroups,omitempty"`
	// AllowedSubdomains restricts the subdomains of USER, GROUP and SERVER
	// sessions. Required with Persistence: every subdomain of an owner gets
	// a volume of its own, and an open list would let users create volumes
	// without bound.
	AllowedSubdomains []string `json:"allowedSubdomains,omitempty"`
	// Persistence gives every engine key of the profile its own
	// PersistentVolumeClaim (Kubernetes provider only).
	Persistence *PersistenceConfig `json:"persistence,omitempty"`
	// Lifecycle overrides (nil = engine.* defaults).
	StartupTimeout *Duration `json:"startupTimeout,omitempty"`
	IdleTimeout    *Duration `json:"idleTimeout,omitempty"`
	MaxLifetime    *Duration `json:"maxLifetime,omitempty"`
	// Kubernetes pod shaping.
	Resources          corev1.ResourceRequirements   `json:"resources,omitempty"`
	Env                []corev1.EnvVar               `json:"env,omitempty"`
	NodeSelector       map[string]string             `json:"nodeSelector,omitempty"`
	Tolerations        []corev1.Toleration           `json:"tolerations,omitempty"`
	Affinity           *corev1.Affinity              `json:"affinity,omitempty"`
	ServiceAccountName string                        `json:"serviceAccountName,omitempty"`
	ImagePullSecrets   []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
	PriorityClassName  string                        `json:"priorityClassName,omitempty"`
	Labels             map[string]string             `json:"labels,omitempty"`
	Annotations        map[string]string             `json:"annotations,omitempty"`
	// PodTemplate is an optional base pod; the gateway adds everything it
	// needs (labels, token, probes, the engine container) on top of it.
	PodTemplate *corev1.PodTemplateSpec `json:"podTemplate,omitempty"`
}

// PoolConfig mirrors kyuubi.engine.pool.size / selectPolicy.
type PoolConfig struct {
	Size int `json:"size,omitempty"`
	// SelectPolicy is RANDOM or POLLING (round robin).
	SelectPolicy string `json:"selectPolicy,omitempty"`
}

// WarmPoolConfig keeps pre-started engines for CONNECTION share level.
type WarmPoolConfig struct {
	Size int `json:"size,omitempty"`
}

// PersistenceConfig gives every engine key of a profile (every user, group
// or subdomain) a PersistentVolumeClaim of its own, mounted at MountPath.
// The gateway creates the claim before the first engine of the key and keeps
// it when engines end. There is never more than one engine per key: a
// successor starts only after its predecessor's pod is gone.
type PersistenceConfig struct {
	// Size is the storage request of new claims, e.g. 10Gi (required).
	Size resource.Quantity `json:"size"`
	// StorageClassName of new claims; empty means the cluster's default.
	StorageClassName string `json:"storageClassName,omitempty"`
	// AccessMode of new claims: ReadWriteOncePod (default) or ReadWriteOnce.
	AccessMode corev1.PersistentVolumeAccessMode `json:"accessMode,omitempty"`
	// MountPath of the volume in the engine container (default /data).
	MountPath string `json:"mountPath,omitempty"`
	// Labels and Annotations are added to new claims (e.g. for backups).
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// Persistence defaults.
const (
	DefaultPersistenceMountPath = "/data"
	// DefaultPersistentDatabase is the database file in the mount path.
	DefaultPersistentDatabase = "engine.duckdb"
)

// subdomainRE restricts subdomains to label-safe names, like Kyuubi's
// [a-zA-Z_0-9-]{1,14} rule but a little more permissive in length.
var subdomainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// ValidSubdomain reports whether s is an acceptable subdomain.
func ValidSubdomain(s string) bool { return subdomainRE.MatchString(s) }

// AuditConfig controls the query audit log.
type AuditConfig struct {
	// Enabled logs every PREPARE_REQUEST (user, session, engine, SQL).
	Enabled bool `json:"enabled"`
	// MaxQueryBytes truncates logged SQL (default 4096).
	MaxQueryBytes int `json:"maxQueryBytes,omitempty"`
}

// HAConfig enables leader election (only the leader serves traffic).
type HAConfig struct {
	Enabled       bool     `json:"enabled"`
	LeaseName     string   `json:"leaseName,omitempty"`
	Namespace     string   `json:"namespace,omitempty"`
	LeaseDuration Duration `json:"leaseDuration,omitempty"`
	RenewDeadline Duration `json:"renewDeadline,omitempty"`
	RetryPeriod   Duration `json:"retryPeriod,omitempty"`
	// DeregistrationDelay is how long a leader that shuts down keeps its
	// Quack listener open after it removed its leader label: until
	// kube-proxy has caught up, it still sends new connections to the pod,
	// which then get an answer at once instead of hanging in connect once
	// the pod is gone. It counts towards server.shutdownTimeout and must be
	// shorter (default 3s, at most half of server.shutdownTimeout; 0s turns
	// it off).
	DeregistrationDelay Duration `json:"deregistrationDelay,omitempty"`
}

// LogConfig controls logging.
type LogConfig struct {
	// Level is debug, info, warn or error.
	Level string `json:"level,omitempty"`
	// Format is "json" or "text".
	Format string `json:"format,omitempty"`
}

// Load reads, defaults and validates a configuration file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse decodes YAML, applies defaults and validates.
func Parse(raw []byte) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// setDur applies a default unless the duration was configured explicitly
// (an explicit 0 means "disabled" where the field documents it).
func setDur(d *Duration, def time.Duration) {
	if !d.IsSet() {
		*d = D(def)
	}
}

func setStr(s *string, def string) {
	if *s == "" {
		*s = def
	}
}

func setInt(i *int, def int) {
	if *i == 0 {
		*i = def
	}
}

// DefaultProfileName is used when no profile is configured or requested.
const DefaultProfileName = "default"

// DefaultEngineImage is the published image built from images/engine: DuckDB
// 2.0.0 for this release (scripts/set-version.sh, docs/releasing.md).
const DefaultEngineImage = "ghcr.io/lldeters/duckdb-engine:2.0.0-0.1.0"

// ApplyDefaults fills unset fields. Zero durations that mean "disabled" are
// only defaulted where documented.
func (c *Config) ApplyDefaults() {
	s := &c.Server
	setStr(&s.Listen, ":9494")
	setInt(&s.MaxHandshakeBytes, 64<<10)
	setDur(&s.ReadHeaderTimeout, 30*time.Second)
	setDur(&s.IdleTimeout, 300*time.Second)
	setDur(&s.ShutdownTimeout, 30*time.Second)
	setStr(&s.Banner, "This is a quackmux gateway for DuckDB Quack. Use ATTACH 'quack:...' to connect here.\n")
	if s.AllowedOrigins == nil {
		if c.Auth.Mode == "none" {
			s.AllowedOrigins = []string{}
		} else {
			s.AllowedOrigins = []string{"*"}
		}
	}
	u := &s.Upstream
	setDur(&u.DialTimeout, 5*time.Second)
	setInt(&u.MaxIdleConnsPerEngine, 64)
	setDur(&u.IdleConnTimeout, 90*time.Second)
	setDur(&u.ControlTimeout, 10*time.Second)
	setDur(&u.HandshakeTimeout, 30*time.Second)
	if u.MaxBufferedRequestBytes == 0 {
		u.MaxBufferedRequestBytes = 100 << 20
	}
	if u.MaxBufferedResponseBytes == 0 {
		u.MaxBufferedResponseBytes = 64 << 20
	}
	if u.BufferBudgetBytes == 0 {
		u.BufferBudgetBytes = 1 << 30
	}
	if u.MaxRequestBytes == 0 {
		u.MaxRequestBytes = 100 << 20
	}

	if c.Admin.TokenFile == "" {
		// Without a token the admin API (which can close sessions and stop
		// engines) is only reachable from this machine unless configured
		// otherwise.
		setStr(&c.Admin.Listen, "127.0.0.1:9495")
	} else {
		setStr(&c.Admin.Listen, ":9495")
	}

	setStr(&c.Auth.Mode, "static")
	setStr(&c.Auth.None.DefaultUser, "anonymous")
	setDur(&c.Auth.Static.ReloadInterval, 10*time.Second)
	j := &c.Auth.JWT
	setStr(&j.UserClaim, "sub")
	setStr(&j.GroupsClaim, "groups")
	setDur(&j.Leeway, 30*time.Second)
	setDur(&j.JWKSRefresh, 15*time.Minute)

	d := &c.Defaults
	setStr(&d.ShareLevel, "USER")
	setStr(&d.Subdomain, "default")
	setStr(&d.Profile, DefaultProfileName)

	ss := &c.Session
	setDur(&ss.IdleTimeout, 6*time.Hour)
	setDur(&ss.MaxLifetime, 0)
	setDur(&ss.MinHeartbeatTimeout, 5*time.Second)
	setDur(&ss.MaxHeartbeatTimeout, 300*time.Second)
	setDur(&ss.HeartbeatGrace, 15*time.Second)
	setDur(&ss.BackendHeartbeatTimeout, time.Hour)
	setDur(&ss.TombstoneTTL, time.Hour)
	setDur(&ss.CheckInterval, 5*time.Second)

	e := &c.Engine
	setDur(&e.StartupTimeout, 180*time.Second)
	if e.IdleTimeout == nil {
		e.IdleTimeout = map[string]Duration{}
	}
	norm := map[string]Duration{}
	for k, v := range e.IdleTimeout {
		norm[strings.ToUpper(k)] = v
	}
	e.IdleTimeout = norm
	for lvl, def := range map[string]time.Duration{"USER": 30 * time.Minute, "GROUP": 30 * time.Minute, "SERVER": 30 * time.Minute} {
		if _, ok := e.IdleTimeout[lvl]; !ok {
			e.IdleTimeout[lvl] = D(def)
		}
	}
	if _, ok := e.IdleTimeout["CONNECTION"]; !ok {
		e.IdleTimeout["CONNECTION"] = D(0)
	}
	setDur(&e.MaxLifetime, 0)
	setDur(&e.DrainTimeout, time.Hour)
	// Long enough for every client to send at least one heartbeat (DuckDB
	// heartbeats every lease/3 with +-20% jitter).
	setDur(&e.RecoveryGrace, ss.MaxHeartbeatTimeout.Duration/2+30*time.Second)
	setDur(&e.CheckInterval, 5*time.Second)
	if e.ReplaceOnSpecChange == nil {
		t := true
		e.ReplaceOnSpecChange = &t
	}
	setDur(&e.FailedRetention, 10*time.Minute)

	l := &c.Limits
	setInt(&l.MaxStartingEngines, 20)

	p := &c.Provider
	setStr(&p.Type, "kubernetes")
	k := &p.Kubernetes
	setStr(&k.Instance, "default")
	setStr(&k.NamePrefix, "quack")
	setDur(&k.DeleteGracePeriod, 30*time.Second)
	if k.QPS == 0 {
		k.QPS = 20
	}
	setInt(&k.Burst, 40)
	pr := &p.Process
	setStr(&pr.DuckDBPath, "duckdb")
	setStr(&pr.BindHost, "127.0.0.1")
	setInt(&pr.PortRangeStart, 19500)
	setInt(&pr.PortRangeEnd, 19999)
	setDur(&p.Static.HealthInterval, 10*time.Second)
	setInt(&p.Static.FailureThreshold, 3)

	if len(c.Profiles) == 0 {
		c.Profiles = map[string]Profile{DefaultProfileName: {}}
	}
	for name, prof := range c.Profiles {
		// Image and ImagePullPolicy stay empty: the Kubernetes provider
		// takes them from the profile's podTemplate, else the defaults.
		if ps := prof.Persistence; ps != nil {
			setStr(&ps.MountPath, DefaultPersistenceMountPath)
			if ps.AccessMode == "" {
				ps.AccessMode = corev1.ReadWriteOncePod
			}
			setStr(&prof.Database, path.Join(ps.MountPath, DefaultPersistentDatabase))
		}
		setStr(&prof.Database, ":memory:")
		setInt(&prof.Pool.Size, 1)
		setStr(&prof.Pool.SelectPolicy, "RANDOM")
		prof.Pool.SelectPolicy = strings.ToUpper(prof.Pool.SelectPolicy)
		c.Profiles[name] = prof
	}

	setInt(&c.Audit.MaxQueryBytes, 4096)

	h := &c.HA
	setStr(&h.LeaseName, "quackmux-"+k.Instance)
	setDur(&h.LeaseDuration, 15*time.Second)
	setDur(&h.RenewDeadline, 10*time.Second)
	setDur(&h.RetryPeriod, 2*time.Second)
	// The delay counts towards server.shutdownTimeout: by default 3s, but
	// at most half of it, so that a short shutdown timeout stays valid
	// without the setting and leaves time to drain.
	setDur(&h.DeregistrationDelay, min(3*time.Second, max(s.ShutdownTimeout.Duration, 0)/2))

	setStr(&c.Log.Level, "info")
	setStr(&c.Log.Format, "json")
}

var validShareLevels = map[string]bool{"CONNECTION": true, "USER": true, "GROUP": true, "SERVER": true}

// Validate checks the configuration for consistency.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	switch c.Auth.Mode {
	case "none":
	case "static":
		if c.Auth.Static.File == "" {
			add("auth.static.file is required for auth.mode=static")
		}
	case "jwt":
		j := c.Auth.JWT
		if j.HMACSecretFile == "" && len(j.PublicKeyFiles) == 0 && j.JWKSURL == "" {
			add("auth.jwt needs hmacSecretFile, publicKeyFiles or jwksURL")
		}
	default:
		add("auth.mode must be none, static or jwt, got %q", c.Auth.Mode)
	}

	checkLevel := func(field, v string) {
		if !validShareLevels[strings.ToUpper(v)] {
			add("%s: unknown share level %q", field, v)
		}
	}
	checkLevel("defaults.shareLevel", c.Defaults.ShareLevel)
	if _, ok := c.Profiles[c.Defaults.Profile]; !ok {
		add("defaults.profile %q is not defined in profiles", c.Defaults.Profile)
	}
	for i, o := range c.Overrides {
		if len(o.Users) == 0 && len(o.Groups) == 0 {
			add("overrides[%d] matches nobody (set users or groups)", i)
		}
		if o.Defaults.ShareLevel != "" {
			checkLevel(fmt.Sprintf("overrides[%d].defaults.shareLevel", i), o.Defaults.ShareLevel)
		}
		if o.Defaults.Profile != "" {
			if _, ok := c.Profiles[o.Defaults.Profile]; !ok {
				add("overrides[%d].defaults.profile %q is not defined", i, o.Defaults.Profile)
			}
		}
		for _, l := range o.AllowedShareLevels {
			checkLevel(fmt.Sprintf("overrides[%d].allowedShareLevels", i), l)
		}
		for _, p := range o.AllowedProfiles {
			if _, ok := c.Profiles[p]; !ok {
				add("overrides[%d].allowedProfiles: %q is not defined", i, p)
			}
		}
	}
	for lvl := range c.Engine.IdleTimeout {
		checkLevel("engine.idleTimeout", lvl)
	}
	defaultsErrs, _ := c.checkDefaults()
	errs = append(errs, defaultsErrs...)

	ss := c.Session
	if ss.MinHeartbeatTimeout.Duration < time.Second {
		add("session.minHeartbeatTimeout must be at least 1s")
	}
	if ss.MaxHeartbeatTimeout.Duration < ss.MinHeartbeatTimeout.Duration {
		add("session.maxHeartbeatTimeout must be >= session.minHeartbeatTimeout")
	}
	if ss.MaxHeartbeatTimeout.Duration > 65535*time.Second {
		add("session.maxHeartbeatTimeout must not exceed 65535s")
	}
	if ss.BackendHeartbeatTimeout.Duration < 30*time.Second {
		add("session.backendHeartbeatTimeout must be at least 30s")
	}
	if ss.CheckInterval.Duration <= 0 || c.Engine.CheckInterval.Duration <= 0 {
		add("session.checkInterval and engine.checkInterval must be positive")
	}
	if c.Engine.RecoveryGrace.Duration < ss.MaxHeartbeatTimeout.Duration*2/5 {
		add("engine.recoveryGrace (%s) must cover at least one heartbeat interval (>= 0.4 * session.maxHeartbeatTimeout)", c.Engine.RecoveryGrace.Duration)
	}

	switch c.Provider.Type {
	case "kubernetes":
	case "process":
		pr := c.Provider.Process
		if pr.PortRangeStart <= 0 || pr.PortRangeEnd > 65535 || pr.PortRangeStart > pr.PortRangeEnd {
			add("provider.process port range %d-%d is invalid", pr.PortRangeStart, pr.PortRangeEnd)
		}
	case "static":
		if len(c.Provider.Static.Engines) == 0 {
			add("provider.static.engines must not be empty")
		}
		seen := map[string]bool{}
		for i, e := range c.Provider.Static.Engines {
			if e.Name == "" || e.URL == "" {
				add("provider.static.engines[%d] needs name and url", i)
			}
			if seen[e.Name] {
				add("provider.static.engines[%d]: duplicate name %q", i, e.Name)
			}
			seen[e.Name] = true
			if e.Token == "" && e.TokenFile == "" {
				add("provider.static.engines[%d] needs token or tokenFile", i)
			}
			checkLevel(fmt.Sprintf("provider.static.engines[%d].shareLevel", i), e.ShareLevel)
			if strings.EqualFold(e.ShareLevel, "CONNECTION") {
				add("provider.static.engines[%d]: CONNECTION share level needs a provider that can launch engines", i)
			}
		}
	default:
		add("provider.type must be kubernetes, process or static, got %q", c.Provider.Type)
	}

	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := c.Profiles[name]
		if name == "" || strings.ContainsAny(name, "/#?&= ") {
			add("profile name %q is invalid", name)
		}
		if p.Pool.Size < 1 || p.Pool.Size > 64 {
			add("profiles.%s.pool.size must be between 1 and 64", name)
		}
		if p.Pool.SelectPolicy != "RANDOM" && p.Pool.SelectPolicy != "POLLING" {
			add("profiles.%s.pool.selectPolicy must be RANDOM or POLLING", name)
		}
		if p.WarmPool.Size < 0 || p.WarmPool.Size > 64 {
			add("profiles.%s.warmPool.size must be between 0 and 64", name)
		}
		for _, l := range p.AllowedShareLevels {
			checkLevel(fmt.Sprintf("profiles.%s.allowedShareLevels", name), l)
		}
		for _, s := range p.AllowedSubdomains {
			if !ValidSubdomain(s) {
				add("profiles.%s.allowedSubdomains: %q is not a valid subdomain (lower-case letters, digits and '-', at most 32 characters)", name, s)
			}
		}
		if p.Persistence != nil {
			errs = append(errs, validatePersistence(name, p, c.Provider.Type)...)
		}
		for k := range p.Settings {
			if !isIdentifier(k) {
				add("profiles.%s.settings: %q is not a valid setting name", name, k)
			}
		}
	}

	if c.HA.Enabled && c.Provider.Type != "kubernetes" {
		add("ha.enabled requires provider.type=kubernetes")
	}
	if c.HA.Enabled && c.Server.SessionKeyFile == "" && os.Getenv("QUACKMUX_SESSION_KEY") == "" {
		add("ha.enabled requires a shared session key (server.sessionKeyFile or QUACKMUX_SESSION_KEY)")
	}
	if d := c.HA.DeregistrationDelay.Duration; d < 0 {
		add("ha.deregistrationDelay must not be negative")
	} else if c.HA.Enabled && d > 0 && d >= c.Server.ShutdownTimeout.Duration {
		// The delay counts towards the shutdown deadline; the drain of the
		// in-flight requests follows it.
		add("ha.deregistrationDelay (%s) must be shorter than server.shutdownTimeout (%s)", d, c.Server.ShutdownTimeout.Duration)
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		add("log.level must be debug, info, warn or error")
	}
	return errors.Join(errs...)
}

// validatePersistence checks a profile with persistence: only keys that are
// reused (USER, GROUP, SERVER) and only one engine per key.
func validatePersistence(name string, p Profile, providerType string) []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	ps := p.Persistence
	if providerType != "kubernetes" {
		add("profiles.%s.persistence needs provider.type kubernetes", name)
	}
	if len(p.AllowedShareLevels) == 0 {
		add("profiles.%s.allowedShareLevels must be set with persistence (USER, GROUP and/or SERVER)", name)
	}
	for _, l := range p.AllowedShareLevels {
		if strings.EqualFold(l, "CONNECTION") {
			add("profiles.%s.allowedShareLevels: CONNECTION cannot be used with persistence (every session would get a volume of its own)", name)
		}
	}
	if len(p.AllowedSubdomains) == 0 {
		add("profiles.%s.allowedSubdomains must be set with persistence (every subdomain gets volumes of its own)", name)
	}
	if p.Pool.Size != 1 {
		add("profiles.%s.pool.size must be 1 with persistence (only one engine may open the data of a key)", name)
	}
	if p.WarmPool.Size != 0 {
		add("profiles.%s.warmPool.size must be 0 with persistence (warm engines serve CONNECTION sessions)", name)
	}
	if ps.Size.Sign() <= 0 {
		add("profiles.%s.persistence.size is required (for example 10Gi)", name)
	}
	switch ps.AccessMode {
	case corev1.ReadWriteOncePod, corev1.ReadWriteOnce:
	default:
		add("profiles.%s.persistence.accessMode must be ReadWriteOncePod or ReadWriteOnce, got %q", name, ps.AccessMode)
	}
	if mp := ps.MountPath; !path.IsAbs(mp) || path.Clean(mp) != mp || mp == "/" || mp == "/tmp" {
		add("profiles.%s.persistence.mountPath %q must be a clean absolute path other than / and /tmp", name, mp)
	}
	return errs
}

func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

// EngineIdleTimeout resolves the idle timeout for a share level and profile.
func (c *Config) EngineIdleTimeout(level, profile string) time.Duration {
	if p, ok := c.Profiles[profile]; ok && p.IdleTimeout != nil {
		return p.IdleTimeout.Duration
	}
	return c.Engine.IdleTimeout[strings.ToUpper(level)].Duration
}

// EngineStartupTimeout resolves the startup timeout for a profile.
func (c *Config) EngineStartupTimeout(profile string) time.Duration {
	if p, ok := c.Profiles[profile]; ok && p.StartupTimeout != nil {
		return p.StartupTimeout.Duration
	}
	return c.Engine.StartupTimeout.Duration
}

// EngineMaxLifetime resolves the max lifetime for a profile.
func (c *Config) EngineMaxLifetime(profile string) time.Duration {
	if p, ok := c.Profiles[profile]; ok && p.MaxLifetime != nil {
		return p.MaxLifetime.Duration
	}
	return c.Engine.MaxLifetime.Duration
}
