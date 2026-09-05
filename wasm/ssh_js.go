// Tailserve extensions to Tailscale's browser SSH client.
// SPDX-License-Identifier: BSD-3-Clause
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"syscall/js"
	"time"

	"golang.org/x/crypto/ssh"
)

// Only the separately built test module installs a transport override.
var testDial func(context.Context, string) (net.Conn, error)

func stringOption(v js.Value, key string) string {
	if x := v.Get(key); x.Type() == js.TypeString {
		return x.String()
	}
	return ""
}
func boolOption(v js.Value, key string, fallback bool) bool {
	if x := v.Get(key); x.Type() == js.TypeBoolean {
		return x.Bool()
	}
	return fallback
}
func callback(v js.Value, key string, args ...any) {
	if f := v.Get(key); f.Type() == js.TypeFunction {
		f.Invoke(args...)
	}
}

// Called from a goroutine, never block the JavaScript event callback itself.
func awaitJS(ctx context.Context, f js.Value, args ...any) (js.Value, error) {
	if f.Type() != js.TypeFunction {
		return js.Undefined(), errors.New("required authentication callback is missing")
	}
	type result struct {
		value js.Value
		err   error
	}
	done := make(chan result, 1)
	var ok, fail js.Func
	ok = js.FuncOf(func(_ js.Value, a []js.Value) any {
		value := js.Undefined()
		if len(a) > 0 {
			value = a[0]
		}
		select {
		case done <- result{value, nil}:
		default:
		}
		return nil
	})
	fail = js.FuncOf(func(_ js.Value, a []js.Value) any {
		message := "authentication prompt cancelled or failed"
		if len(a) > 0 && a[0].Type() == js.TypeObject {
			if m := a[0].Get("message"); m.Type() == js.TypeString {
				message = m.String()
			}
		}
		select {
		case done <- result{js.Undefined(), errors.New(message)}:
		default:
		}
		return nil
	})
	promise := js.Global().Get("Promise").Call("resolve", f.Invoke(args...))
	promise.Call("then", ok, fail)
	select {
	case r := <-done:
		ok.Release()
		fail.Release()
		return r.value, r.err
	case <-ctx.Done():
		// The UI aborts its pending prompt when onDone fires. Release after settlement.
		go func() { <-done; ok.Release(); fail.Release() }()
		return js.Undefined(), ctx.Err()
	}
}

func (s *jsSSHSession) Run() {
	cfg := s.termConfig
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	defer cancel()
	defer func() {
		for _, f := range s.callbacks {
			f.Release()
		}
	}()
	defer callback(cfg, "onDone")
	fail := func(label string, err error) {
		if !s.closed {
			callback(cfg, "writeErrorFn", fmt.Sprintf("%s: %v", label, err))
		}
	}
	port := 22
	if p := cfg.Get("port"); p.Type() == js.TypeNumber {
		port = p.Int()
	}
	if port < 1 || port > 65535 {
		fail("SSH", errors.New("invalid port"))
		return
	}
	address := net.JoinHostPort(s.host, strconv.Itoa(port))
	callback(cfg, "onConnectionProgress", "Connecting to "+s.host+"…")
	dialCtx, dialCancel := context.WithTimeout(ctx, 20*time.Second)
	var c net.Conn
	var err error
	if testDial != nil {
		c, err = testDial(dialCtx, address)
	} else {
		c, err = s.jsIPN.dialer.UserDial(dialCtx, "tcp", address)
	}
	dialCancel()
	if err != nil {
		fail("Dial", err)
		return
	}
	s.conn = c
	defer c.Close()
	if s.closed {
		return
	}
	// Auth dialogs have a bounded lifetime; deadlines are cleared after the handshake.
	authCtx, authCancel := context.WithTimeout(ctx, 180*time.Second)
	defer authCancel()
	c.SetDeadline(time.Now().Add(180 * time.Second))
	hostKeyReceived := false
	config := &ssh.ClientConfig{User: s.username, BannerCallback: func(message string) error {
		callback(cfg, "onBanner", message)
		return nil
	}, HostKeyCallback: func(host string, remote net.Addr, key ssh.PublicKey) error {
		hostKeyReceived = true
		if stringOption(cfg, "authentication") == "tailscale" {
			return nil
		}
		fingerprint := ssh.FingerprintSHA256(key)
		expected := stringOption(cfg, "fingerprint")
		if expected != "" {
			if expected != fingerprint {
				return errors.New("SSH host fingerprint changed; connection refused")
			}
			return nil
		}
		answer, err := awaitJS(authCtx, cfg.Get("verifyHost"), fingerprint)
		if err != nil {
			return fmt.Errorf("SSH host verification: %w", err)
		}
		if answer.Type() != js.TypeBoolean || !answer.Bool() {
			return errors.New("SSH host key was not trusted")
		}
		return nil
	}}
	if stringOption(cfg, "authentication") != "tailscale" {
		if raw := stringOption(cfg, "privateKey"); raw != "" {
			var signer ssh.Signer
			if pass := stringOption(cfg, "passphrase"); pass != "" {
				signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(raw), []byte(pass))
			} else {
				signer, err = ssh.ParsePrivateKey([]byte(raw))
			}
			if err != nil {
				fail("SSH key", errors.New("invalid private key or key passphrase"))
				return
			}
			config.Auth = append(config.Auth, ssh.PublicKeys(signer))
		}
		if password := stringOption(cfg, "password"); password != "" {
			config.Auth = append(config.Auth, ssh.Password(password))
		}
		if cfg.Get("keyboardInteractive").Type() == js.TypeFunction {
			config.Auth = append(config.Auth, ssh.RetryableAuthMethod(ssh.KeyboardInteractive(func(user, instruction string, questions []string, echos []bool) ([]string, error) {
				prompts := make([]any, len(questions))
				for i, q := range questions {
					prompts[i] = map[string]any{"prompt": q, "echo": echos[i]}
				}
				answer, err := awaitJS(authCtx, cfg.Get("keyboardInteractive"), map[string]any{"name": user, "instructions": instruction, "prompts": prompts})
				if err != nil {
					return nil, err
				}
				if answer.Type() != js.TypeObject || answer.Length() != len(questions) {
					return nil, errors.New("invalid verification response")
				}
				out := make([]string, len(questions))
				for i := range out {
					out[i] = answer.Index(i).String()
				}
				return out, nil
			}), 3))
		}
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(c, address, config)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || strings.HasSuffix(err.Error(), ": EOF") {
			stage := "before SSH host verification"
			if hostKeyReceived {
				stage = "during SSH sign-in"
			}
			fail("SSH connection", fmt.Errorf("%s closed the connection %s for user %q. Check any server message above and the server's SSH logs. (%w)", address, stage, s.username, err))
		} else {
			fail("SSH handshake", err)
		}
		return
	}
	c.SetDeadline(time.Time{})
	authCancel()
	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		fail("SSH session", err)
		return
	}
	s.session = session
	defer session.Close()
	stdin, err := session.StdinPipe()
	if err != nil {
		fail("SSH stdin", err)
		return
	}
	session.Stdout = rawWriter{cfg.Get("writeFn")}
	if cfg.Get("stderrFn").Type() == js.TypeFunction {
		session.Stderr = rawWriter{cfg.Get("stderrFn")}
	} else {
		session.Stderr = session.Stdout
	}
	input := make(chan string, 128)
	readFn := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) == 0 || s.closed {
			return nil
		}
		// Never block a JS callback on remote network backpressure.
		select {
		case input <- args[0].String():
		default:
			fail("SSH input", errors.New("input queue full; reconnect before retrying"))
			s.Close()
		}
		return nil
	})
	defer readFn.Release()
	callback(cfg, "setReadFn", readFn)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case text := <-input:
				if _, err := io.WriteString(stdin, text); err != nil {
					if !s.closed {
						fail("SSH input", err)
					}
					return
				}
			}
		}
	}()
	if boolOption(cfg, "pty", true) {
		rows, cols := 24, 80
		if v := cfg.Get("rows"); v.Type() == js.TypeNumber {
			rows = v.Int()
		}
		if v := cfg.Get("cols"); v.Type() == js.TypeNumber {
			cols = v.Int()
		}
		if s.pendingResizeRows > 0 {
			rows = s.pendingResizeRows
		}
		if s.pendingResizeCols > 0 {
			cols = s.pendingResizeCols
		}
		if err = session.RequestPty("xterm-256color", rows, cols, ssh.TerminalModes{}); err != nil {
			fail("SSH terminal", err)
			return
		}
	}
	if command := stringOption(cfg, "command"); command != "" {
		err = session.Start(command)
	} else {
		err = session.Shell()
	}
	if err != nil {
		fail("SSH command", err)
		return
	}
	callback(cfg, "onConnected")
	err = session.Wait()
	code := 0
	if err != nil {
		var exit *ssh.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitStatus()
		} else {
			code = -1
		}
	}
	callback(cfg, "onExit", code)
	if err != nil && cfg.Get("onExit").Type() != js.TypeFunction {
		fail("SSH session", err)
	}
}

type rawWriter struct{ f js.Value }

func (w rawWriter) Write(p []byte) (int, error) {
	if w.f.Type() == js.TypeFunction {
		data := js.Global().Get("Uint8Array").New(len(p))
		js.CopyBytesToJS(data, p)
		w.f.Invoke(data)
	}
	return len(p), nil
}

func validateKey(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return map[string]any{"error": "Private key required"}
	}
	raw := args[0].String()
	pass := ""
	if len(args) > 1 {
		pass = args[1].String()
	}
	if len(raw) > 64000 {
		return map[string]any{"error": "Private key too large"}
	}
	var signer ssh.Signer
	var err error
	if pass != "" {
		signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(raw), []byte(pass))
	} else {
		signer, err = ssh.ParsePrivateKey([]byte(raw))
	}
	if err != nil {
		return map[string]any{"error": "Invalid private key or passphrase"}
	}
	return map[string]any{"fingerprint": ssh.FingerprintSHA256(signer.PublicKey()), "type": signer.PublicKey().Type()}
}
