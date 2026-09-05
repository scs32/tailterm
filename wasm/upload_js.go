// Browser file transfers share the SSH authentication and host verification path.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
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
	if _, err = remote.Lstat(destination); err == nil {
		return errors.New("a file already exists at that path; choose another filename")
	} else if !os.IsNotExist(err) {
		return err
	}
	token := make([]byte, 12)
	if _, err = rand.Read(token); err != nil {
		return err
	}
	temporary := path.Join(path.Dir(destination), ".tailterm-upload-"+hex.EncodeToString(token)+".part")
	file, err := remote.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		file.Close()
		if !complete {
			remote.Remove(temporary)
		}
	}()
	if err = file.Chmod(0600); err != nil {
		return err
	}
	for offset := 0; offset < size; {
		if err = ctx.Err(); err != nil {
			return err
		}
		length := min(32768, size-offset)
		data, e := awaitJS(ctx, cfg.Get("readChunk"), offset, length)
		if e != nil {
			return e
		}
		if data.Type() != js.TypeObject || data.Get("byteLength").Int() != length {
			return errors.New("invalid image chunk")
		}
		chunk := make([]byte, length)
		js.CopyBytesToGo(chunk, data)
		n, e := file.Write(chunk)
		if e != nil {
			return e
		}
		if n != length {
			return errors.New("incomplete file write")
		}
		offset += n
		callback(cfg, "progress", offset)
	}
	if err = file.Close(); err != nil {
		return err
	}
	// Standard SFTP rename refuses to replace an existing destination. Do not use PosixRename.
	if err = remote.Rename(temporary, destination); err != nil {
		return fmt.Errorf("could not publish uploaded file (it may already exist): %w", err)
	}
	complete = true
	return nil
}
