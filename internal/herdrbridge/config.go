package herdrbridge

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjpiazza/sandherd/internal/lifecycle"
)

type Config struct {
	BaseURL        string                  `json:"baseUrl"`
	TokenFile      string                  `json:"tokenFile"`
	Create         lifecycle.CreateRequest `json:"create"`
	PollInterval   time.Duration           `json:"-"`
	ReconnectLimit time.Duration           `json:"-"`
}

func LoadConfig() (Config, error) {
	path := os.Getenv("SANDHERD_CONFIG_FILE")
	if path == "" {
		configurationDirectory := os.Getenv("HERDR_PLUGIN_CONFIG_DIR")
		if configurationDirectory == "" {
			return Config{}, fmt.Errorf("HERDR_PLUGIN_CONFIG_DIR or SANDHERD_CONFIG_FILE is required")
		}
		path = filepath.Join(configurationDirectory, "config.json")
	}
	contents, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Config{}, fmt.Errorf("read configuration %s: %w", path, err)
	}
	var configuration Config
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&configuration); err != nil {
		return Config{}, fmt.Errorf("decode configuration %s: %w", path, err)
	}
	if value := os.Getenv("SANDHERD_BASE_URL"); value != "" {
		configuration.BaseURL = value
	}
	if value := os.Getenv("SANDHERD_TOKEN_FILE"); value != "" {
		configuration.TokenFile = value
	}
	configuration.PollInterval = time.Second
	configuration.ReconnectLimit = 2 * time.Minute
	if err := configuration.Validate(); err != nil {
		return Config{}, err
	}
	return configuration, nil
}

func (c Config) Validate() error {
	parsed, err := url.Parse(c.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("baseUrl must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if c.TokenFile == "" {
		return fmt.Errorf("tokenFile is required")
	}
	if c.Create.Name != "" {
		return fmt.Errorf("create.name must be omitted; the manager prompts for it")
	}
	request := c.Create
	request.Name = "validation"
	request.ApplyDefaults()
	if err := request.Validate(); err != nil {
		return fmt.Errorf("create defaults are invalid: %w", err)
	}
	if c.PollInterval <= 0 || c.ReconnectLimit <= 0 {
		return fmt.Errorf("poll and reconnect durations must be positive")
	}
	return nil
}

func (c Config) CreateRequest(name string) lifecycle.CreateRequest {
	request := c.Create
	request.Name = name
	request.ApplyDefaults()
	return request
}

func StateDirectory() (string, error) {
	directory := os.Getenv("HERDR_PLUGIN_STATE_DIR")
	if directory == "" {
		return "", fmt.Errorf("HERDR_PLUGIN_STATE_DIR is required")
	}
	return filepath.Clean(directory), nil
}
