package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type toolServer struct {
	Name   string   `json:"name"`
	Auth   string   `json:"authStatus"`
	Status string   `json:"runtimeStatus"`
	Tools  []string `json:"tools"`
}
type toolInventory struct {
	Host     string       `json:"host"`
	Runtime  string       `json:"runtime"`
	Version  string       `json:"version"`
	Scope    string       `json:"scope"`
	Servers  []toolServer `json:"servers"`
	Commands []string     `json:"commands"`
	Notes    []string     `json:"notes"`
}

// Only names and status leave the host. Never expose server URLs, environment,
// tokens, tool schemas or raw configuration in the browser inventory.
func readCodexInventory(ctx context.Context, codex, thread string, proxy bool) ([]toolServer, error) {
	args := []string{"app-server", "--stdio"}
	if proxy {
		args = []string{"app-server", "proxy"}
	}
	cmd := exec.CommandContext(ctx, codex, args...)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		in.Close()
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			cmd.Process.Kill()
			<-done
		}
	}()
	enc := json.NewEncoder(in)
	dec := json.NewDecoder(io.LimitReader(out, 8<<20))
	request := func(id int, method string, params any) (json.RawMessage, error) {
		if err := enc.Encode(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			return nil, err
		}
		for {
			var v struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := dec.Decode(&v); err != nil {
				return nil, err
			}
			if v.ID == id {
				if len(v.Error) > 0 {
					return nil, errors.New("Codex inventory request was rejected")
				}
				return v.Result, nil
			}
		}
	}
	if _, err = request(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "tailterm_tools", "version": "1"}, "capabilities": map[string]bool{"experimentalApi": true}}); err != nil {
		return nil, err
	}
	if err = enc.Encode(map[string]string{"method": "initialized"}); err != nil {
		return nil, err
	}
	var result []toolServer
	cursor := ""
	for page := 0; page < 10; page++ {
		params := map[string]any{"detail": "toolsAndAuthOnly", "limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if thread != "" {
			params["threadId"] = thread
		}
		raw, err := request(page+2, "mcpServerStatus/list", params)
		if err != nil {
			return nil, err
		}
		var response struct {
			Data []struct {
				Name   string                     `json:"name"`
				Auth   string                     `json:"authStatus"`
				Status string                     `json:"runtimeStatus"`
				Tools  map[string]json.RawMessage `json:"tools"`
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		if err = json.Unmarshal(raw, &response); err != nil {
			return nil, err
		}
		for _, s := range response.Data {
			tools := []string{}
			for name := range s.Tools {
				tools = append(tools, name)
			}
			sort.Strings(tools)
			result = append(result, toolServer{Name: s.Name, Auth: s.Auth, Status: s.Status, Tools: tools})
		}
		cursor = response.NextCursor
		if cursor == "" {
			return result, nil
		}
	}
	return nil, errors.New("inventory exceeded pagination limit")
}
func cmdTools(args []string) error {
	fs := flag.NewFlagSet("tools", flag.ContinueOnError)
	runtime := fs.String("runtime", "codex", "agent app")
	cwd := fs.String("cwd", "", "project directory for inspection")
	asJSON := fs.Bool("json", false, "structured inventory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !strings.Contains("|codex|claude|gemini|aider|", "|"+*runtime+"|") {
		return errors.New("choose codex, claude, gemini or aider")
	}
	if *cwd != "" {
		if !filepath.IsAbs(*cwd) {
			return errors.New("working directory must be absolute")
		}
		if err := os.Chdir(*cwd); err != nil {
			return errors.New("working directory is unavailable")
		}
	}
	hostname, _ := os.Hostname()
	report := toolInventory{Host: hostname, Runtime: *runtime, Scope: "host", Servers: []toolServer{}, Commands: []string{}, Notes: []string{}}
	for _, name := range []string{"tt", "tmux", "git", "rg", "node", "npm", "python3", "go", "docker", "ssh"} {
		if _, err := exec.LookPath(name); err == nil {
			report.Commands = append(report.Commands, name)
		}
	}
	binary, err := exec.LookPath(*runtime)
	if err != nil {
		return fmt.Errorf("%s is not installed on this host", *runtime)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	versionCtx, versionCancel := context.WithTimeout(ctx, 3*time.Second)
	version, versionErr := exec.CommandContext(versionCtx, binary, "--version").Output()
	versionCancel()
	if versionErr == nil && len(version) < 256 {
		report.Version = strings.TrimSpace(string(version))
	}
	if *runtime == "codex" {
		thread := os.Getenv("CODEX_THREAD_ID")
		if threadIDPattern.MatchString(thread) {
			probe, stop := context.WithTimeout(ctx, 4*time.Second)
			report.Servers, err = readCodexInventory(probe, binary, thread, true)
			stop()
			if err == nil {
				report.Scope = "thread"
			}
		}
		if report.Scope != "thread" {
			report.Servers, err = readCodexInventory(ctx, binary, "", false)
		}
		if err != nil {
			report.Servers = []toolServer{}
			report.Notes = append(report.Notes, "MCP inventory unavailable: the installed Codex version or service could not answer within the inspection timeout.")
		}
	} else {
		report.Notes = append(report.Notes, "This app does not yet have a structured MCP inventory adapter in Tailterm. Its built-in and connector tools are not verified here.")
	}
	if report.Scope == "host" {
		report.Notes = append(report.Notes, "Host inventory, not a launched agent's tool manifest. Project settings, plugins and permissions may change the tools available at launch.")
	}
	report.Notes = append(report.Notes, "Tool names and authentication status do not guarantee a successful call. Listed shell commands are installed; sandbox access and service credentials may still block them.")
	if *asJSON {
		printJSON(report)
	} else {
		fmt.Printf("%s on %s (%s inventory)\nInstalled commands: %s\n", report.Runtime, report.Host, report.Scope, strings.Join(report.Commands, ", "))
		for _, s := range report.Servers {
			fmt.Printf("%s: %s; auth=%s; %d tools\n", s.Name, s.Status, s.Auth, len(s.Tools))
			for _, t := range s.Tools {
				fmt.Println("  " + t)
			}
		}
		for _, note := range report.Notes {
			fmt.Println(note)
		}
	}
	return nil
}
