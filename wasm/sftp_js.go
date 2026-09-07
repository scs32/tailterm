// A persistent SFTP session for the Files view. It shares the SSH connection,
// authentication, and host verification with terminals and uploads.
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
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const (
	sftpChunk     = 32 * 1024
	sftpMaxList   = 10000
	sftpMaxWrite  = 4 << 30
	sftpMaxPath   = 4096
	sftpMaxDownlo = 4 << 30
)

func validRemotePath(p string) error {
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\x00\r\n") || len(p) > sftpMaxPath {
		return errors.New("paths must be absolute and free of control characters")
	}
	return nil
}

func fileEntry(info os.FileInfo, name string) map[string]any {
	mode := info.Mode()
	return map[string]any{
		"name":   name,
		"size":   info.Size(),
		"mode":   mode.String(),
		"isDir":  mode.IsDir(),
		"isLink": mode&os.ModeSymlink != 0,
		"mtime":  info.ModTime().UnixMilli(),
	}
}

// sftpSession opens the subsystem and hands JavaScript an operations object.
// It returns when the context ends or JavaScript calls close().
func sftpSession(ctx context.Context, client *ssh.Client, cfg js.Value) error {
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
	remote, err := sftp.NewClientPipe(reader, uploadPipe{writer, session})
	if err != nil {
		return fmt.Errorf("server must enable SFTP: %w", err)
	}
	defer remote.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var funcs []js.Func
	fn := func(f func(args []js.Value) (any, error)) js.Value {
		jf := js.FuncOf(func(_ js.Value, args []js.Value) any {
			return makePromise(func() (any, error) {
				if ctx.Err() != nil {
					return nil, errors.New("SFTP session closed")
				}
				return f(args)
			})
		})
		funcs = append(funcs, jf)
		return jf.Value
	}
	pathArg := func(args []js.Value, i int) (string, error) {
		if len(args) <= i || args[i].Type() != js.TypeString {
			return "", errors.New("path required")
		}
		p := args[i].String()
		if err := validRemotePath(p); err != nil {
			return "", err
		}
		return p, nil
	}
	ops := map[string]any{
		"home": fn(func(args []js.Value) (any, error) {
			return remote.RealPath(".")
		}),
		"realpath": fn(func(args []js.Value) (any, error) {
			p, err := pathArg(args, 0)
			if err != nil {
				return nil, err
			}
			return remote.RealPath(p)
		}),
		"list": fn(func(args []js.Value) (any, error) {
			p, err := pathArg(args, 0)
			if err != nil {
				return nil, err
			}
			infos, err := remote.ReadDir(p)
			if err != nil {
				return nil, err
			}
			out := make([]any, 0, len(infos))
			for i, info := range infos {
				if i >= sftpMaxList {
					break
				}
				out = append(out, fileEntry(info, info.Name()))
			}
			return map[string]any{"path": p, "entries": out, "truncated": len(infos) > sftpMaxList}, nil
		}),
		"stat": fn(func(args []js.Value) (any, error) {
			p, err := pathArg(args, 0)
			if err != nil {
				return nil, err
			}
			info, err := remote.Stat(p)
			if err != nil {
				return nil, err
			}
			return fileEntry(info, path.Base(p)), nil
		}),
		"mkdir": fn(func(args []js.Value) (any, error) {
			p, err := pathArg(args, 0)
			if err != nil {
				return nil, err
			}
			return nil, remote.Mkdir(p)
		}),
		"rename": fn(func(args []js.Value) (any, error) {
			from, err := pathArg(args, 0)
			if err != nil {
				return nil, err
			}
			to, err := pathArg(args, 1)
			if err != nil {
				return nil, err
			}
			if _, err := remote.Lstat(to); err == nil {
				return nil, errors.New("something already exists at the destination")
			}
			// Standard SFTP rename never replaces an existing destination.
			return nil, remote.Rename(from, to)
		}),
		"remove": fn(func(args []js.Value) (any, error) {
			p, err := pathArg(args, 0)
			if err != nil {
				return nil, err
			}
			info, err := remote.Lstat(p)
			if err != nil {
				return nil, err
			}
			if info.IsDir() {
				// Only empty directories; no recursive deletion from the browser.
				return nil, remote.RemoveDirectory(p)
			}
			return nil, remote.Remove(p)
		}),
		// read(path, onChunk) streams the file to JavaScript in 32 KiB pieces.
		"read": fn(func(args []js.Value) (any, error) {
			p, err := pathArg(args, 0)
			if err != nil {
				return nil, err
			}
			if len(args) < 2 || args[1].Type() != js.TypeFunction {
				return nil, errors.New("chunk callback required")
			}
			info, err := remote.Stat(p)
			if err != nil {
				return nil, err
			}
			if info.IsDir() {
				return nil, errors.New("that is a directory")
			}
			if info.Size() > sftpMaxDownlo {
				return nil, errors.New("file is too large to download in the browser")
			}
			file, err := remote.Open(p)
			if err != nil {
				return nil, err
			}
			defer file.Close()
			buf := make([]byte, sftpChunk)
			var total int64
			uint8Array := js.Global().Get("Uint8Array")
			for {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				n, readErr := file.Read(buf)
				if n > 0 {
					chunk := uint8Array.New(n)
					js.CopyBytesToJS(chunk, buf[:n])
					if _, err := awaitJS(ctx, args[1], chunk, total); err != nil {
						return nil, err
					}
					total += int64(n)
				}
				if readErr == io.EOF {
					break
				}
				if readErr != nil {
					return nil, readErr
				}
			}
			return total, nil
		}),
		// write(path, {size, readChunk, progress, overwrite}) uploads through a
		// private temporary file and publishes it with a non-replacing rename.
		"write": fn(func(args []js.Value) (any, error) {
			p, err := pathArg(args, 0)
			if err != nil {
				return nil, err
			}
			if len(args) < 2 || args[1].Type() != js.TypeObject {
				return nil, errors.New("write options required")
			}
			return nil, putFile(ctx, remote, p, args[1], sftpMaxWrite)
		}),
	}
	closed := make(chan struct{})
	closeFn := js.FuncOf(func(_ js.Value, _ []js.Value) any {
		select {
		case <-closed:
		default:
			close(closed)
		}
		return nil
	})
	funcs = append(funcs, closeFn)
	ops["close"] = closeFn.Value
	callback(cfg, "onReady", js.ValueOf(ops))
	select {
	case <-closed:
	case <-ctx.Done():
	}
	cancel()
	// Give in-flight promises a moment to observe cancellation before
	// releasing the callbacks they may still reference.
	time.Sleep(50 * time.Millisecond)
	for _, f := range funcs {
		f.Release()
	}
	return nil
}

// putFile writes size bytes pulled from JavaScript into a temporary file next
// to the destination, then renames it into place.
func putFile(ctx context.Context, remote *sftp.Client, destination string, cfg js.Value, maxSize int) error {
	if v := cfg.Get("size"); v.Type() != js.TypeNumber {
		return errors.New("invalid upload size")
	}
	size := cfg.Get("size").Int()
	if size < 0 || size > maxSize {
		return fmt.Errorf("files must be at most %d bytes", maxSize)
	}
	overwrite := boolOption(cfg, "overwrite", false)
	if _, err := remote.Lstat(destination); err == nil && !overwrite {
		return errors.New("a file already exists at that path; choose another filename")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	token := make([]byte, 12)
	if _, err := rand.Read(token); err != nil {
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
		length := min(sftpChunk, size-offset)
		data, e := awaitJS(ctx, cfg.Get("readChunk"), offset, length)
		if e != nil {
			return e
		}
		if data.Type() != js.TypeObject || data.Get("byteLength").Int() != length {
			return errors.New("invalid file chunk")
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
	if overwrite {
		if err = remote.PosixRename(temporary, destination); err != nil {
			return fmt.Errorf("could not replace the file: %w", err)
		}
	} else if err = remote.Rename(temporary, destination); err != nil {
		return fmt.Errorf("could not publish uploaded file (it may already exist): %w", err)
	}
	complete = true
	return nil
}
