package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Default storage follows the host's configured plugin directory, including mounted paths.
// Only the plugin directory is read; host credentials and other settings are never retained.
func defaultStorageDir(args []string, cwd string) string {
	configPath := "config.yaml"
	for i, arg := range args {
		if (arg == "--config" || arg == "-config") && i+1 < len(args) {
			configPath = args[i+1]
		}
		for _, prefix := range []string{"--config=", "-config="} {
			if strings.HasPrefix(arg, prefix) {
				configPath = strings.TrimPrefix(arg, prefix)
			}
		}
	}
	if !filepath.IsAbs(configPath) {
		configPath = filepath.Join(cwd, configPath)
	}
	pluginDir := "plugins"
	if file, err := os.Open(configPath); err == nil {
		var hint struct {
			Plugins struct {
				Dir string `yaml:"dir"`
			} `yaml:"plugins"`
		}
		raw, errRead := io.ReadAll(io.LimitReader(file, (8<<20)+1))
		_ = file.Close()
		if errRead == nil && len(raw) <= 8<<20 && yaml.Unmarshal(raw, &hint) == nil && strings.TrimSpace(hint.Plugins.Dir) != "" {
			pluginDir = strings.TrimSpace(hint.Plugins.Dir)
		}
	}
	if pluginDir == "~" || strings.HasPrefix(pluginDir, "~/") || strings.HasPrefix(pluginDir, `~\`) {
		if homeDir, err := os.UserHomeDir(); err == nil {
			if pluginDir == "~" {
				pluginDir = homeDir
			} else {
				pluginDir = filepath.Join(homeDir, pluginDir[2:])
			}
		}
	}
	if !filepath.IsAbs(pluginDir) {
		pluginDir = filepath.Join(cwd, pluginDir)
	}
	return filepath.Join(pluginDir, "."+pluginName)
}

func (state *runtimeState) applyStorageDefaults(cfg *pluginConfig) error {
	if cfg.StateFile == "" {
		root := state.storageDir
		if root == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			root = defaultStorageDir(os.Args[1:], cwd)
		}
		cfg.StateFile = filepath.Join(root, "current.json")
	}
	if cfg.SelectionFile == "" {
		cfg.SelectionFile = cfg.StateFile + ".selection.json"
	}
	if cfg.ProxyPoolFile == "" {
		cfg.ProxyPoolFile = cfg.StateFile + ".proxies.json"
	}
	if cfg.RuntimeFile == "" {
		cfg.RuntimeFile = cfg.StateFile + ".runtime.json"
	}
	if cfg.ArchiveDir == "" {
		cfg.ArchiveDir = filepath.Join(filepath.Dir(cfg.StateFile), "archive")
	}
	return nil
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled: true, MaxStateBytes: defaultMaxBytes, Mode: "force", SelectionRequired: true,
		Defaults: &credentialConfig{AcceptedBlocks: []int{10, 12}, Models: []string{"gpt-6-astra", "gpt-5.6-sol"}},
		Probe:    probeConfig{Enabled: true, Continuous: true, OnMissing: true, ReasoningEffort: "medium", RetrySeconds: 5, RefreshBeforeSeconds: 300, MaxAttempts: 1, QuotaBackoffSeconds: 900},
	}
}
