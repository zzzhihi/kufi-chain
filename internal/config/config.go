// Package config provides configuration loading and management
package config

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
)

// Config represents the complete application configuration
type Config struct {
	Server      ServerConfig      `mapstructure:"server"`
	Fabric      FabricConfig      `mapstructure:"fabric"`
	Transaction TransactionConfig `mapstructure:"transaction"`
	WorkerPool  WorkerPoolConfig  `mapstructure:"worker_pool"`
	Receipt     ReceiptConfig     `mapstructure:"receipt"`
	Collections CollectionsConfig `mapstructure:"collections"`
	Policies    PoliciesConfig    `mapstructure:"endorsement_policies"`
	Logging     LoggingConfig     `mapstructure:"logging"`
	Audit       AuditConfig       `mapstructure:"audit"`
	APIKey      APIKeyConfig      `mapstructure:"api_key"`
	DataDir     string            `mapstructure:"data_dir"`
}

// ServerConfig holds HTTP server settings
type ServerConfig struct {
	Host            string        `mapstructure:"host"`
	Port            int           `mapstructure:"port"`
	ReadTimeout     time.Duration `mapstructure:"read_timeout"`
	WriteTimeout    time.Duration `mapstructure:"write_timeout"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
	MTLS            MTLSConfig    `mapstructure:"mtls"`
}

// MTLSConfig holds mTLS settings
type MTLSConfig struct {
	Enabled  bool   `mapstructure:"enabled"`
	CertFile string `mapstructure:"cert_file"`
	KeyFile  string `mapstructure:"key_file"`
	CAFile   string `mapstructure:"ca_file"`
}

// FabricConfig holds Hyperledger Fabric connection settings
type FabricConfig struct {
	MSPID         string          `mapstructure:"msp_id"`
	CertPath      string          `mapstructure:"cert_path"`
	KeyPath       string          `mapstructure:"key_path"`
	TLS           TLSConfig       `mapstructure:"tls"`
	Peers         []PeerConfig    `mapstructure:"peers"`
	ChannelName   string          `mapstructure:"channel_name"`
	Chaincode     ChaincodeConfig `mapstructure:"chaincode"`
	Timeouts      TimeoutsConfig  `mapstructure:"timeouts"`
	Retry         RetryConfig     `mapstructure:"retry"`
	MaxConcurrent int             `mapstructure:"max_concurrent"`
}

// APIKeyConfig holds API key authentication settings
type APIKeyConfig struct {
	Enabled bool     `mapstructure:"enabled"`
	Keys    []string `mapstructure:"keys"`
}

// TLSConfig holds TLS settings
type TLSConfig struct {
	Enabled      bool   `mapstructure:"enabled"`
	RootCertPath string `mapstructure:"root_cert_path"`
}

// PeerConfig holds peer endpoint configuration
type PeerConfig struct {
	Name              string `mapstructure:"name"`
	Endpoint          string `mapstructure:"endpoint"`
	TLSCertPath       string `mapstructure:"tls_cert_path"`
	OverrideAuthority string `mapstructure:"override_authority"`
}

// ChaincodeConfig holds chaincode settings
type ChaincodeConfig struct {
	Name    string `mapstructure:"name"`
	Version string `mapstructure:"version"`
}

// TimeoutsConfig holds various timeout settings
type TimeoutsConfig struct {
	Connect      time.Duration `mapstructure:"connect"`
	Endorse      time.Duration `mapstructure:"endorse"`
	Submit       time.Duration `mapstructure:"submit"`
	CommitStatus time.Duration `mapstructure:"commit_status"`
	Evaluate     time.Duration `mapstructure:"evaluate"`
}

// TransactionConfig holds transaction processing settings
type TransactionConfig struct {
	IdempotencyTTL    time.Duration `mapstructure:"idempotency_ttl"`
	NonceWindow       time.Duration `mapstructure:"nonce_window"`
	HighRiskThreshold int64         `mapstructure:"high_risk_threshold"`
	RateLimit         int           `mapstructure:"rate_limit"`
	RateLimitWindow   time.Duration `mapstructure:"rate_limit_window"`
	Retry             RetryConfig   `mapstructure:"retry"`
}

// RetryConfig holds retry settings
type RetryConfig struct {
	MaxAttempts       int           `mapstructure:"max_attempts"`
	InitialBackoff    time.Duration `mapstructure:"initial_backoff"`
	MaxBackoff        time.Duration `mapstructure:"max_backoff"`
	BackoffMultiplier float64       `mapstructure:"backoff_multiplier"`
}

// WorkerPoolConfig holds worker pool settings
type WorkerPoolConfig struct {
	Size      int `mapstructure:"size"`
	QueueSize int `mapstructure:"queue_size"`
}

// ReceiptConfig holds receipt generation settings
type ReceiptConfig struct {
	IncludeFullCerts bool   `mapstructure:"include_full_certs"`
	StatusEndpoint   string `mapstructure:"status_endpoint"`
}

// CollectionsConfig holds private data collection names
type CollectionsConfig struct {
	IntraBank string `mapstructure:"intra_bank"`
	InterBank string `mapstructure:"inter_bank"`
	Regulator string `mapstructure:"regulator"`
}

// PoliciesConfig holds endorsement policy identifiers
type PoliciesConfig struct {
	IntraBankStandard string `mapstructure:"intra_bank_standard"`
	InterBankStandard string `mapstructure:"inter_bank_standard"`
	HighRisk          string `mapstructure:"high_risk"`
}

// LoggingConfig holds logging settings
type LoggingConfig struct {
	Level    string `mapstructure:"level"`
	Format   string `mapstructure:"format"`
	Output   string `mapstructure:"output"`
	FilePath string `mapstructure:"file_path"`
}

// AuditConfig holds audit logging settings
type AuditConfig struct {
	Enabled    bool     `mapstructure:"enabled"`
	LogPath    string   `mapstructure:"log_path"`
	MaskFields []string `mapstructure:"mask_fields"`
}

// Load loads configuration from the specified path
func Load(configPath string) (*Config, error) {
	v := viper.New()

	v.SetConfigFile(configPath)
	v.SetConfigType("yaml")

	// Set defaults
	setDefaults(v)

	// Read config file
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Unmarshal to struct
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Validate configuration
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return &cfg, nil
}

// setDefaults sets default configuration values
func setDefaults(v *viper.Viper) {
	v.SetDefault("server.host", "0.0.0.0")
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.read_timeout", "30s")
	v.SetDefault("server.write_timeout", "30s")
	v.SetDefault("server.shutdown_timeout", "10s")

	v.SetDefault("fabric.timeouts.connect", "10s")
	v.SetDefault("fabric.timeouts.endorse", "30s")
	v.SetDefault("fabric.timeouts.submit", "30s")
	v.SetDefault("fabric.timeouts.commit_status", "60s")
	v.SetDefault("fabric.timeouts.evaluate", "10s")
	v.SetDefault("fabric.retry.max_attempts", 3)
	v.SetDefault("fabric.retry.initial_backoff", "200ms")
	v.SetDefault("fabric.retry.max_backoff", "10s")
	v.SetDefault("fabric.retry.backoff_multiplier", 2.0)
	v.SetDefault("fabric.max_concurrent", 64)

	v.SetDefault("api_key.enabled", false)
	v.SetDefault("transaction.idempotency_ttl", "24h")
	v.SetDefault("transaction.nonce_window", "5m")
	v.SetDefault("transaction.high_risk_threshold", 1000000000)
	v.SetDefault("transaction.rate_limit", 20000)
	v.SetDefault("transaction.rate_limit_window", "1m")
	v.SetDefault("transaction.retry.max_attempts", 3)
	v.SetDefault("transaction.retry.initial_backoff", "100ms")
	v.SetDefault("transaction.retry.max_backoff", "5s")
	v.SetDefault("transaction.retry.backoff_multiplier", 2.0)

	v.SetDefault("worker_pool.size", 16)
	v.SetDefault("worker_pool.queue_size", 256)

	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "json")
	v.SetDefault("logging.output", "stdout")

	v.SetDefault("data_dir", "./data")
}

// Validate validates the configuration
func (c *Config) Validate() error {
	if c.Fabric.MSPID == "" {
		return fmt.Errorf("fabric.msp_id is required")
	}
	if c.Fabric.ChannelName == "" {
		return fmt.Errorf("fabric.channel_name is required")
	}
	if c.Fabric.Chaincode.Name == "" {
		return fmt.Errorf("fabric.chaincode.name is required")
	}
	if len(c.Fabric.Peers) == 0 {
		return fmt.Errorf("at least one peer must be configured")
	}
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("invalid server port: %d", c.Server.Port)
	}
	return nil
}

// GetAddress returns the server address string
func (c *Config) GetAddress() string {
	return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port)
}
