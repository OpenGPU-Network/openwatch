package config

import (
	"errors"
	"strings"

	"github.com/spf13/viper"
)

// Default values. Defined as constants so every reader of the config (tests,
// docs, the watcher) shares one source of truth — no drift between defaults
// listed in viper.SetDefault and the values written in the Dockerfile / PRD.
const (
	DefaultInterval           = 86400
	DefaultHealthcheckTimeout = 30
	DefaultStopTimeout        = 10
	DefaultLogLevel           = "info"
	DefaultLogFormat          = "text"
)

type Config struct {
	Interval           int    `mapstructure:"interval"`
	Schedule           string `mapstructure:"schedule"`
	Cleanup            bool   `mapstructure:"cleanup"`
	IncludeStopped     bool   `mapstructure:"include_stopped"`
	LabelEnable        bool   `mapstructure:"label_enable"`
	RollbackOnFailure  bool   `mapstructure:"rollback_on_failure"`
	HealthcheckTimeout int    `mapstructure:"healthcheck_timeout"`
	StopTimeout        int    `mapstructure:"stop_timeout"`
	NotifyURL          string `mapstructure:"notify_url"`
	LogLevel           string `mapstructure:"log_level"`
	LogFormat          string `mapstructure:"log_format"`
	HTTPAPI            bool   `mapstructure:"http_api"`

	// DockerConfig is the path to Docker's config.json (the file normally at
	// ~/.docker/config.json). Bound to the standard Docker env var
	// DOCKER_CONFIG so private registry auth works the same way as the
	// Docker CLI. Empty string means "use the default location".
	DockerConfig string `mapstructure:"docker_config"`

	// RegistryUser and RegistryPassword provide direct registry credentials
	// that bypass config.json entirely. Useful when the host's Docker config
	// uses a credential helper that is unavailable inside the container
	// (e.g. docker-credential-desktop on macOS). These apply to all
	// registries unless overridden by config.json for a specific host.
	RegistryUser     string `mapstructure:"registry_user"`
	RegistryPassword string `mapstructure:"registry_password"`
}

// Config file search paths, tried in order. The first file that exists
// and parses successfully is used. Missing files are silently ignored;
// a malformed file is a hard error.
var configSearchPaths = []string{
	"/etc/openwatch",
}

// configFileName is the base name of the YAML config file, sans
// extension. Viper appends .yaml/.yml based on SetConfigType.
const configFileName = "openwatch"

func Load() (*Config, error) {
	v := viper.New()

	v.SetDefault("interval", DefaultInterval)
	v.SetDefault("schedule", "")
	v.SetDefault("cleanup", false)
	v.SetDefault("include_stopped", false)
	v.SetDefault("label_enable", false)
	v.SetDefault("rollback_on_failure", false)
	v.SetDefault("healthcheck_timeout", DefaultHealthcheckTimeout)
	v.SetDefault("stop_timeout", DefaultStopTimeout)
	v.SetDefault("notify_url", "")
	v.SetDefault("log_level", DefaultLogLevel)
	v.SetDefault("log_format", DefaultLogFormat)
	v.SetDefault("http_api", false)
	v.SetDefault("docker_config", "")
	v.SetDefault("registry_user", "")
	v.SetDefault("registry_password", "")

	v.SetEnvPrefix("OPENWATCH")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Explicit bindings so env vars work reliably regardless of map key style.
	_ = v.BindEnv("interval", "OPENWATCH_INTERVAL")
	_ = v.BindEnv("schedule", "OPENWATCH_SCHEDULE")
	_ = v.BindEnv("cleanup", "OPENWATCH_CLEANUP")
	_ = v.BindEnv("include_stopped", "OPENWATCH_INCLUDE_STOPPED")
	_ = v.BindEnv("label_enable", "OPENWATCH_LABEL_ENABLE")
	_ = v.BindEnv("rollback_on_failure", "OPENWATCH_ROLLBACK_ON_FAILURE")
	_ = v.BindEnv("healthcheck_timeout", "OPENWATCH_HEALTHCHECK_TIMEOUT")
	_ = v.BindEnv("stop_timeout", "OPENWATCH_STOP_TIMEOUT")
	_ = v.BindEnv("notify_url", "OPENWATCH_NOTIFY_URL")
	_ = v.BindEnv("log_level", "OPENWATCH_LOG_LEVEL")
	_ = v.BindEnv("log_format", "OPENWATCH_LOG_FORMAT")
	_ = v.BindEnv("http_api", "OPENWATCH_HTTP_API")
	_ = v.BindEnv("registry_user", "OPENWATCH_REGISTRY_USER")
	_ = v.BindEnv("registry_password", "OPENWATCH_REGISTRY_PASSWORD")

	// DOCKER_CONFIG is a standard Docker env var (no OPENWATCH_ prefix). We
	// route it through viper rather than calling os.Getenv from auth.go so
	// all external config reads flow through a single chokepoint.
	_ = v.BindEnv("docker_config", "DOCKER_CONFIG")

	// Load optional YAML file. Viper's precedence is
	// flag > env > config > default, so env bindings above already take
	// precedence over anything we read from the file — that's exactly
	// the priority order the PRD asks for. A missing file is fine; only
	// parse errors propagate upward.
	v.SetConfigName(configFileName)
	v.SetConfigType("yaml")
	for _, p := range configSearchPaths {
		v.AddConfigPath(p)
	}
	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return nil, err
		}
	}

	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
