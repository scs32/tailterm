package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSavedTokenNeverFollowsAnotherHubURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "tailterm")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hub.json"), []byte(`{"url":"http://private:18765","token":"saved-secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	e := env{}
	e.loadConfig()
	if e.hub != "http://private:18765" || e.token != "saved-secret" {
		t.Fatal("saved config not loaded")
	}
	e.hub = "http://another:18765"
	e.loadConfig()
	if e.token != "" {
		t.Fatal("saved token leaked to another URL")
	}
}
