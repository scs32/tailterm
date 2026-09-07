package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// A host-local credential keeps the token out of SSH command arguments.
func (e *env) loadConfig() {
	if e.configHub != "" && e.configHub != e.hub {
		e.token = ""
		e.configHub = ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "tailterm", "hub.json"))
	if err != nil {
		return
	}
	var config struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	}
	if json.Unmarshal(data, &config) != nil {
		return
	}
	if e.hub == "" {
		e.hub = config.URL
	}
	if e.hub == config.URL && e.token == "" {
		e.token = config.Token
		e.configHub = config.URL
	}
}
