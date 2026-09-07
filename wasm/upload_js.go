// Browser file transfers share the SSH authentication and host verification path.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall/js"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type uploadPipe struct {
	io.Writer
	session *ssh.Session
}

func (p uploadPipe) Close() error { return p.session.Close() }

func uploadFile(ctx context.Context, client *ssh.Client, cfg js.Value) error {
	destination := stringOption(cfg, "path")
	if !strings.HasPrefix(destination, "/") || strings.ContainsAny(destination, "\x00\r\n") || len(destination) > 4096 {
		return errors.New("choose an absolute destination path")
	}
	if v := cfg.Get("size"); v.Type() != js.TypeNumber {
		return errors.New("invalid upload size")
	}
	size := cfg.Get("size").Int()
	if size < 1 || size > 20*1024*1024 {
		return errors.New("images must be between 1 byte and 20 MiB")
	}
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	reader, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	writer, err := session.StdinPipe()
	if err != nil {
		return err
	}
	if err = session.RequestSubsystem("sftp"); err != nil {
		return fmt.Errorf("server must enable SFTP: %w", err)
	}
	// Close the whole SSH subsystem channel, not just stdin, so completion
	// cannot wait indefinitely for a server to respond to a half-close.
	remote, err := sftp.NewClientPipe(reader, uploadPipe{writer, session})
	if err != nil {
		return fmt.Errorf("server must enable SFTP: %w", err)
	}
	defer remote.Close()
	return putFile(ctx, remote, destination, cfg, 20*1024*1024)
}
