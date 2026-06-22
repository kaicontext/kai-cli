package safetygate

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// configFileName is the per-repo gate config, sibling of db.sqlite
// inside the kai data directory.
const configFileName = "gate.yaml"

// LoadConfig reads gate.yaml from the kai data directory. Callers
// typically pass `kaipath.Resolve(cwd)`. Missing file → DefaultConfig.
// A malformed file is an error: silent fallback would mask config drift.
func LoadConfig(kaiDir string) (Config, error) {
	p := filepath.Join(kaiDir, configFileName)
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultConfig(), nil
		}
		return Config{}, fmt.Errorf("reading %s: %w", p, err)
	}
	cfg := DefaultConfig()
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing %s: %w", p, err)
	}
	if cfg.BlockThreshold == 0 {
		cfg.BlockThreshold = DefaultConfig().BlockThreshold
	}
	return cfg, nil
}
