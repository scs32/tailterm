// Command replay stands in for a runtime in a private tmux pane for
// TestRuntimePromptTmuxReplay. It draws frame-N.txt from a frames folder,
// moves between frames on Up and Down, draws after.txt on Enter, and appends
// the name of every key it receives to a log. The test builds it under the
// runtime's executable name (codex or claude) so process discovery finds it.
//
// Usage: replay FRAMES_DIR KEY_LOG START_FRAME
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: replay FRAMES_DIR KEY_LOG START_FRAME")
		os.Exit(2)
	}
	dir, logPath := os.Args[1], os.Args[2]
	at, _ := strconv.Atoi(os.Args[3])
	stty := exec.Command("stty", "-icanon", "-echo", "min", "1", "time", "0")
	stty.Stdin = os.Stdin
	if err := stty.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "stty:", err)
		os.Exit(1)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(1)
	}
	draw := func(name string) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return
		}
		os.Stdout.WriteString("\x1b[H\x1b[2J" + strings.TrimRight(string(data), "\n"))
	}
	frame := func(i int) string { return fmt.Sprintf("frame-%d.txt", i) }
	draw(frame(at))
	// One read may hold several keys (tmux sends "Down Down" together).
	buf := make([]byte, 64)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return
		}
		for input := string(buf[:n]); input != ""; {
			key, size := "", 1
			switch {
			case strings.HasPrefix(input, "\x1b[B"), strings.HasPrefix(input, "\x1bOB"):
				key, size = "Down", 3
				if _, err := os.Stat(filepath.Join(dir, frame(at+1))); err == nil {
					at++
				}
				draw(frame(at))
			case strings.HasPrefix(input, "\x1b[A"), strings.HasPrefix(input, "\x1bOA"):
				key, size = "Up", 3
				if at > 0 {
					at--
				}
				draw(frame(at))
			case input[0] == '\r' || input[0] == '\n':
				key = "Enter"
				draw("after.txt")
			default:
				key = fmt.Sprintf("%q", input[:1])
			}
			input = input[size:]
			fmt.Fprintln(logFile, key)
		}
	}
}
