package teamplan

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

//go:embed plan.mjs
var source string

// Run executes the generated browser plan code with a JSON boundary. Neither
// the token nor the prepared item context appears in a shell command line.
func Run(ctx context.Context, input, output any) error {
	node, err := exec.LookPath("node")
	if err != nil {
		return fmt.Errorf("team launch requires Node.js on this host: %w", err)
	}
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, node, "--input-type=module", "-e", source)
	cmd.Stdin = bytes.NewReader(data)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("resolve team plan: %s: %w", strings.TrimSpace(stderr.String()), err)
	}
	if err := json.Unmarshal(stdout.Bytes(), output); err != nil {
		return fmt.Errorf("decode team plan: %w", err)
	}
	return nil
}
