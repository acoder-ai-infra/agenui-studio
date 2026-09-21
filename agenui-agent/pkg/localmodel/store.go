// Package localmodel stores the local user's model credential and publishes a
// secret reference to Harness' managed provider registry. Harness remains the
// only model router; no generated model or Agent configuration is maintained.
package localmodel

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	keyPath       = "var/local-secrets/model-config.key"
	modelKeyEnv   = "AGENUI_LOCAL_MODEL_API_KEY"
	modelProvider = "local_user_model"

	ProtocolOpenAICompatible = "openai_compatible"
	ProtocolAnthropic        = "anthropic"
)

// Config contains the user-owned model connection exposed by the local UI.
// Cache and reasoning policy remain explicit in local configuration, while the
// wire protocol is explicit because OpenAI-compatible and Anthropic endpoints
// use different request paths and payloads.
type Config struct {
	BaseURL  string `json:"baseUrl"`
	Model    string `json:"model"`
	APIKey   string `json:"apiKey,omitempty"`
	Protocol string `json:"protocol"`
}

type Store struct {
	db      *sql.DB
	keyFile string
}

func New(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("local model: database is required")
	}
	s := &Store{db: db, keyFile: keyPath}
	if err := s.EnsureSchema(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) EnsureSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS local_model_config (id INTEGER PRIMARY KEY CHECK(id=1), base_url TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', api_key_ciphertext TEXT NOT NULL DEFAULT '', protocol TEXT NOT NULL DEFAULT 'openai_compatible');`); err != nil {
		return fmt.Errorf("local model schema: %w", err)
	}
	// Upgrade plaintext configurations in place so no clear-text credential
	// remains after schema initialization.
	if err := ensureColumn(ctx, s.db, "local_model_config", "api_key_ciphertext", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	hasProtocol, err := hasColumn(ctx, s.db, "local_model_config", "protocol")
	if err != nil {
		return err
	}
	if !hasProtocol {
		if err := ensureColumn(ctx, s.db, "local_model_config", "protocol", "TEXT NOT NULL DEFAULT 'openai_compatible'"); err != nil {
			return err
		}
		// Configurations without a protocol default to OpenAI-compatible; infer
		// Anthropic only when the endpoint identifies that protocol explicitly.
		if _, err := s.db.ExecContext(ctx, `UPDATE local_model_config SET protocol=? WHERE lower(base_url) LIKE '%anthropic%'`, ProtocolAnthropic); err != nil {
			return fmt.Errorf("local model migrate protocol: %w", err)
		}
	}
	if err := s.migratePlaintext(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS model_providers (
tenant_id TEXT NOT NULL, provider_id TEXT NOT NULL, definition_json TEXT NOT NULL CHECK (json_valid(definition_json)),
revision INTEGER NOT NULL CHECK (revision >= 1), created_at_ms INTEGER NOT NULL CHECK (created_at_ms >= 0),
updated_at_ms INTEGER NOT NULL CHECK (updated_at_ms >= created_at_ms), updated_by TEXT NOT NULL,
identity_digest BLOB NOT NULL CHECK (length(identity_digest) = 32), PRIMARY KEY (tenant_id, provider_id), UNIQUE (identity_digest));
CREATE INDEX IF NOT EXISTS model_providers_tenant_updated_idx ON model_providers (tenant_id, updated_at_ms DESC, provider_id);`); err != nil {
		return fmt.Errorf("local model managed provider schema: %w", err)
	}
	return nil
}

func ensureColumn(ctx context.Context, db *sql.DB, table, name, definition string) error {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return fmt.Errorf("local model inspect schema: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var column, typ string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &column, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if column == name {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+name+" "+definition); err != nil {
		return fmt.Errorf("local model add %s column: %w", name, err)
	}
	return nil
}

func (s *Store) migratePlaintext(ctx context.Context) error {
	hasLegacy, err := hasColumn(ctx, s.db, "local_model_config", "api_key")
	if err != nil || !hasLegacy {
		return err
	}
	var plain, encrypted string
	err = s.db.QueryRowContext(ctx, `SELECT api_key,api_key_ciphertext FROM local_model_config WHERE id=1`).Scan(&plain, &encrypted)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (plain == "" || encrypted != "") {
		return nil
	}
	if err != nil {
		return err
	}
	sealed, err := s.seal(plain)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE local_model_config SET api_key='',api_key_ciphertext=? WHERE id=1`, sealed)
	return err
}

func hasColumn(ctx context.Context, db *sql.DB, table, name string) (bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var column, typ string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &column, &typ, &notNull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if column == name {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) Load(ctx context.Context) (Config, bool, error) {
	var cfg Config
	var sealed string
	err := s.db.QueryRowContext(ctx, `SELECT base_url,model,api_key_ciphertext,protocol FROM local_model_config WHERE id=1`).Scan(&cfg.BaseURL, &cfg.Model, &sealed, &cfg.Protocol)
	if errors.Is(err, sql.ErrNoRows) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, err
	}
	if cfg.BaseURL == "" || cfg.Model == "" || sealed == "" {
		return Config{}, false, nil
	}
	key, err := s.open(sealed)
	if err != nil {
		return Config{}, false, err
	}
	cfg.APIKey = key
	cfg.Protocol = normalizeProtocol(cfg.Protocol)
	return cfg, true, nil
}

func (s *Store) Save(ctx context.Context, cfg Config) error {
	cfg.BaseURL = strings.TrimSpace(cfg.BaseURL)
	cfg.Model = strings.TrimSpace(cfg.Model)
	if cfg.Protocol == "" || cfg.APIKey == "" {
		previous, ok, err := s.Load(ctx)
		if err != nil {
			return err
		}
		if cfg.Protocol == "" {
			if ok {
				cfg.Protocol = previous.Protocol
			} else {
				cfg.Protocol = ProtocolOpenAICompatible
			}
		}
		if cfg.APIKey == "" && !ok {
			return errors.New("apiKey is required for the first save")
		}
		if cfg.APIKey == "" {
			cfg.APIKey = previous.APIKey
		}
	}
	cfg.Protocol = normalizeProtocol(cfg.Protocol)
	if err := validate(cfg); err != nil {
		return err
	}
	sealed, err := s.seal(cfg.APIKey)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO local_model_config(id,base_url,model,api_key_ciphertext,protocol) VALUES(1,?,?,?,?) ON CONFLICT(id) DO UPDATE SET base_url=excluded.base_url,model=excluded.model,api_key_ciphertext=excluded.api_key_ciphertext,protocol=excluded.protocol`, cfg.BaseURL, cfg.Model, sealed, cfg.Protocol)
	if err != nil {
		return err
	}
	if err := os.Setenv(modelKeyEnv, cfg.APIKey); err != nil {
		return fmt.Errorf("local model activate credential: %w", err)
	}
	return s.publishManagedProviders(ctx, cfg)
}

func (s *Store) publishManagedProviders(ctx context.Context, cfg Config) error {
	now := time.Now().UTC().UnixMilli()
	for _, tenantID := range []string{"default", "public"} {
		definition := map[string]any{
			"id": modelProvider, "display_name": "Studio model", "tenant_id": tenantID,
			"scope": "tenant", "version": "1.0.0", "protocol": cfg.Protocol,
			"base_url": cfg.BaseURL, "api_key_env": modelKeyEnv,
			"models": []string{cfg.Model}, "is_default": true, "default_model": cfg.Model,
			"capability": map[string]any{
				"chat": true, "streaming": true, "tool_calling": true,
				"structured_output": true, "json_schema": true,
				"reasoning": true, "reasoning_toggle": true,
				"limits": map[string]int{"max_context_tokens": 128000, "max_output_tokens": 65536},
			},
		}
		encoded, err := json.Marshal(definition)
		if err != nil {
			return err
		}
		digest := sha256.New()
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(tenantID))
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(modelProvider))
		if _, err := s.db.ExecContext(ctx, `INSERT INTO model_providers(
tenant_id,provider_id,definition_json,revision,created_at_ms,updated_at_ms,updated_by,identity_digest)
VALUES(?,?,?,1,?,?,?,?) ON CONFLICT(tenant_id,provider_id) DO UPDATE SET
definition_json=excluded.definition_json,revision=model_providers.revision+1,
updated_at_ms=excluded.updated_at_ms,updated_by=excluded.updated_by`,
			tenantID, modelProvider, encoded, now, now, "studio-settings", digest.Sum(nil)); err != nil {
			return fmt.Errorf("local model publish provider for %s: %w", tenantID, err)
		}
	}
	return nil
}

func normalizeProtocol(protocol string) string {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if protocol == "" {
		return ProtocolOpenAICompatible
	}
	return protocol
}

func validate(cfg Config) error {
	if cfg.BaseURL == "" || cfg.Model == "" {
		return errors.New("baseUrl and model are required")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname()))) {
		return errors.New("baseUrl must be https or an http loopback URL")
	}
	if cfg.Protocol != ProtocolOpenAICompatible && cfg.Protocol != ProtocolAnthropic {
		return errors.New("protocol must be openai_compatible or anthropic")
	}
	return nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Store) seal(plain string) (string, error) {
	key, err := s.masterKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(append(nonce, gcm.Seal(nil, nonce, []byte(plain), nil)...)), nil
}

func (s *Store) open(sealed string) (string, error) {
	raw, err := base64.RawStdEncoding.DecodeString(sealed)
	if err != nil {
		return "", err
	}
	key, err := s.masterKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(raw) < gcm.NonceSize() {
		return "", errors.New("local model credential is invalid")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	return string(plain), err
}

func (s *Store) masterKey() ([]byte, error) {
	key, err := os.ReadFile(s.keyFile)
	if err == nil && len(key) == 32 {
		return key, nil
	}
	if err := os.MkdirAll(filepath.Dir(s.keyFile), 0o700); err != nil {
		return nil, fmt.Errorf("local model create key directory: %w", err)
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("local model generate key: %w", err)
	}
	if err := os.WriteFile(s.keyFile, key, 0o600); err != nil {
		return nil, fmt.Errorf("local model persist key: %w", err)
	}
	return key, nil
}

// Prepared identifies the unchanged Harness composition file. The credential
// is supplied through a process-local environment reference and the provider
// definition is read by Harness from its managed database source.
type Prepared struct {
	HarnessConfigPath string
}

func Prepare(ctx context.Context, db *sql.DB, harnessPath string) (Prepared, bool, error) {
	s, err := New(db)
	if err != nil {
		return Prepared{}, false, err
	}
	cfg, ok, err := s.Load(ctx)
	if err != nil || !ok {
		return Prepared{}, ok, err
	}
	if err := os.Setenv(modelKeyEnv, cfg.APIKey); err != nil {
		return Prepared{}, false, err
	}
	if err := s.publishManagedProviders(ctx, cfg); err != nil {
		return Prepared{}, false, err
	}
	return Prepared{HarnessConfigPath: harnessPath}, true, nil
}
