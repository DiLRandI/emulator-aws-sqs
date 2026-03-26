package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	ListenAddr             string
	PublicBaseURL          string
	Region                 string
	AllowedRegions         []string
	AccountID              string
	SQLitePath             string
	DataSource             string
	AuthMode               string
	CredentialsFile        string
	DefaultAccessKeyID     string
	DefaultSecretAccessKey string
	DefaultSessionToken    string
	AllowInsecureBypass    bool
	LogLevel               string
	CreatePropagation      time.Duration
	DeleteCooldown         time.Duration
	PurgeCooldown          time.Duration
	AttributePropagation   time.Duration
	RetentionPropagation   time.Duration
	StatsRefreshInterval   time.Duration
	ReaperInterval         time.Duration
	MoveTaskPollInterval   time.Duration
	LongPollWakeFrequency  time.Duration
}

func Load() Config {
	sqlitePath := envOr("SQS_SQLITE_PATH", "sqs.db")
	dataSource := envOr("SQS_SQLITE_DSN", sqliteDSN(sqlitePath))

	cfg := Config{
		ListenAddr:             envOr("SQS_LISTEN_ADDR", ":9324"),
		PublicBaseURL:          envOr("SQS_PUBLIC_BASE_URL", "http://127.0.0.1:9324"),
		Region:                 envOr("SQS_REGION", "us-east-1"),
		AllowedRegions:         splitCSV(envOr("SQS_ALLOWED_REGIONS", "us-east-1")),
		AccountID:              envOr("SQS_ACCOUNT_ID", "000000000000"),
		SQLitePath:             sqlitePath,
		DataSource:             dataSource,
		AuthMode:               envOr("SQS_AUTH_MODE", "strict"),
		CredentialsFile:        envOr("SQS_CREDENTIALS_FILE", ""),
		DefaultAccessKeyID:     envOr("SQS_DEFAULT_ACCESS_KEY_ID", "test"),
		DefaultSecretAccessKey: envOr("SQS_DEFAULT_SECRET_ACCESS_KEY", "test"),
		DefaultSessionToken:    envOr("SQS_DEFAULT_SESSION_TOKEN", ""),
		AllowInsecureBypass:    envBool("SQS_ALLOW_INSECURE_BYPASS", false),
		LogLevel:               envOr("SQS_LOG_LEVEL", "INFO"),
		CreatePropagation:      envDuration("SQS_CREATE_PROPAGATION", time.Second),
		DeleteCooldown:         envDuration("SQS_DELETE_COOLDOWN", 60*time.Second),
		PurgeCooldown:          envDuration("SQS_PURGE_COOLDOWN", 60*time.Second),
		AttributePropagation:   envDuration("SQS_ATTRIBUTE_PROPAGATION", 60*time.Second),
		RetentionPropagation:   envDuration("SQS_RETENTION_PROPAGATION", 15*time.Minute),
		StatsRefreshInterval:   envDuration("SQS_STATS_REFRESH_INTERVAL", 5*time.Second),
		ReaperInterval:         envDuration("SQS_REAPER_INTERVAL", time.Second),
		MoveTaskPollInterval:   envDuration("SQS_MOVE_TASK_POLL_INTERVAL", time.Second),
		LongPollWakeFrequency:  envDuration("SQS_LONG_POLL_WAKE_FREQUENCY", 250*time.Millisecond),
	}

	flag.StringVar(&cfg.ListenAddr, "listen", cfg.ListenAddr, "listen address")
	flag.StringVar(&cfg.PublicBaseURL, "public-base-url", cfg.PublicBaseURL, "public base URL for queue URLs")
	flag.StringVar(&cfg.Region, "region", cfg.Region, "default region")
	flag.StringVar(&cfg.AccountID, "account-id", cfg.AccountID, "default local account id")
	flag.StringVar(&cfg.SQLitePath, "sqlite-path", cfg.SQLitePath, "sqlite file path")
	flag.StringVar(&cfg.DataSource, "sqlite-dsn", cfg.DataSource, "sqlite DSN")
	flag.StringVar(&cfg.AuthMode, "auth-mode", cfg.AuthMode, "auth mode: strict or bypass")
	flag.StringVar(&cfg.CredentialsFile, "credentials-file", cfg.CredentialsFile, "credentials file")
	flag.BoolVar(&cfg.AllowInsecureBypass, "allow-insecure-bypass", cfg.AllowInsecureBypass, "allow unsigned requests when auth-mode=bypass")
	flag.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "slog level")
	flag.Parse()

	cfg.PublicBaseURL = strings.TrimRight(cfg.PublicBaseURL, "/")
	if len(cfg.AllowedRegions) == 0 {
		cfg.AllowedRegions = []string{cfg.Region}
	}
	if !flagPassed("sqlite-dsn") && flagPassed("sqlite-path") {
		cfg.DataSource = sqliteDSN(cfg.SQLitePath)
	}
	return cfg
}

func (c Config) Validate() error {
	if c.PublicBaseURL == "" {
		return fmt.Errorf("public base URL is required")
	}
	if c.Region == "" {
		return fmt.Errorf("region is required")
	}
	if c.AccountID == "" {
		return fmt.Errorf("account id is required")
	}
	return nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if raw := os.Getenv(key); raw != "" {
		value, err := time.ParseDuration(raw)
		if err == nil {
			return value
		}
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}

func splitCSV(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func sqliteDSN(path string) string {
	return fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)", path)
}

func flagPassed(name string) bool {
	prefix := "-" + name
	for _, arg := range os.Args[1:] {
		if arg == prefix || strings.HasPrefix(arg, prefix+"=") {
			return true
		}
	}
	return false
}
