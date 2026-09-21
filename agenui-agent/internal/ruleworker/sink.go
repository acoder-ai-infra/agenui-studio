package ruleworker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ArtifactConfig intentionally supports only the bundled filesystem backend.
// External artifact stores can be integrated at the callback boundary without
// becoming a runtime dependency of Studio.
type ArtifactConfig struct {
	Root string `yaml:"root"`
}

func LoadArtifactConfig(path string) (ArtifactConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ArtifactConfig{}, fmt.Errorf("ruleworker: read artifact config %s: %w", path, err)
	}
	var config ArtifactConfig
	if err := yaml.Unmarshal(raw, &config); err != nil {
		return ArtifactConfig{}, fmt.Errorf("ruleworker: decode artifact config %s: %w", path, err)
	}
	return config, nil
}

type Sink interface {
	Put(context.Context, string, []byte) (string, error)
	Root() string
}

func NewSink(config ArtifactConfig) (Sink, error) {
	root := strings.TrimSpace(config.Root)
	if root == "" {
		root = "var/ruleworker/artifacts"
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("ruleworker: resolve artifact directory: %w", err)
	}
	return &fileSink{root: abs}, nil
}

type fileSink struct{ root string }

func (s *fileSink) Put(_ context.Context, key string, data []byte) (string, error) {
	target := filepath.Join(s.root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return "", err
	}
	return target, nil
}

func (s *fileSink) Root() string { return s.root }
