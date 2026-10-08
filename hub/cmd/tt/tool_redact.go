package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Secret redaction (feature wi_79001641d97cd64c) is a step inside the
// tool-call ledger's PostToolUse hook. It looks for recognizable tokens and
// keys in the output of a successful tool call. In report mode it only counts
// them in the ledger row. In redact mode it also answers Claude Code with the
// same output, each value replaced by a placeholder naming its pattern.
//
// It is off unless the host's relay.json says otherwise, it shares the
// ledger's one deadline, and whenever it is slow, unsure or broken the
// original output passes unchanged. Nothing matched, nor any part, length,
// position or hash of it, is written anywhere. See docs/claude-wake.md.

const (
	toolRedactOff    = "off"
	toolRedactReport = "report"
	toolRedactRedact = "redact"

	toolRedactLongest  = 4 << 10 // a longer run of token characters is still replaced whole
	toolRedactMinExact = 16      // shortest host value compared against output
)

// Variables rather than constants so tests can change them.
var (
	toolRedactScanBudget = 100 * time.Millisecond // from the handler's start; then the scan gives up
	// Largest tool_response scanned, in bytes. The Mini scans more than this
	// in 50 ms: TestToolRedactBenchmark measures it.
	toolRedactMaxBytes = 1 << 20
	// toolRedactRewrite builds the redacted output. Only redact mode calls it.
	toolRedactRewrite = redactRewriteValue
)

// toolRedactEnv lists the only environment variables whose values are
// compared against tool output. Changing the list is a code change.
var toolRedactEnv = []string{
	"TAILTERM_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "OPENAI_API_KEY",
	"GITHUB_TOKEN", "GH_TOKEN", "CLOUDFLARE_API_TOKEN", "CLOUDFLARE_API_KEY", "DISCORD_TOKEN", "DISCORD_BOT_TOKEN",
	"TS_AUTHKEY", "TS_API_KEY",
}

// toolRedactMode reads the host setting claudeSecretRedaction from
// ~/.config/tailterm/relay.json. It is read on every tool call, so a change
// applies at each affected session's next call. A missing or unreadable
// file, malformed JSON, a missing key and any value other than "report" or
// "redact" mean off. The key is read by its exact name, as relayStallAction
// reads its own. TAILTERM_REDACT_SETTING_FILE names another file, for tests.
func toolRedactMode() string {
	path := toolRedactSettingPath()
	if path == "" {
		return toolRedactOff
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 64<<10 {
		return toolRedactOff
	}
	var config map[string]json.RawMessage
	var mode string
	if json.Unmarshal(data, &config) != nil || json.Unmarshal(config["claudeSecretRedaction"], &mode) != nil {
		return toolRedactOff
	}
	if mode == toolRedactReport || mode == toolRedactRedact {
		return mode
	}
	return toolRedactOff
}

func toolRedactSettingPath() string {
	if path := os.Getenv("TAILTERM_REDACT_SETTING_FILE"); path != "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "tailterm", "relay.json")
}

// toolRedactExact returns the values this host already holds: the hub token
// tt loaded and the listed environment variables, each only when long enough
// that it cannot match ordinary text. They stay in memory.
func toolRedactExact(token string) []string {
	values := []string{token}
	for _, name := range toolRedactEnv {
		values = append(values, os.Getenv(name))
	}
	var exact []string
	for _, value := range values {
		if len(value) >= toolRedactMinExact {
			exact = append(exact, value)
		}
	}
	return exact
}

// redactSpan is one match inside one string value.
type redactSpan struct {
	start, end int
	name       string
}

// redactClass is a set of ASCII bytes.
type redactClass [256]bool

func newRedactClass(chars string) *redactClass {
	var c redactClass
	for i := 0; i < len(chars); i++ {
		c[chars[i]] = true
	}
	return &c
}

const redactAlnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

var (
	redactW      = newRedactClass(redactAlnum)
	redactWU     = newRedactClass(redactAlnum + "_")
	redactWD     = newRedactClass(redactAlnum + "-")
	redactWUD    = newRedactClass(redactAlnum + "_-")
	redactUpper  = newRedactClass("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
	redactHeader = newRedactClass("ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 ")
	redactBase64 = newRedactClass(redactAlnum + "/+=")
)

// redactShape is a high-confidence token shape: a literal prefix, then a run
// of the class. With exact set, the run must be exactly that long and a
// longer run is a different string. Otherwise the run must be at least min
// long and the whole run is the match, so no tail is left behind.
type redactShape struct {
	name   string
	prefix string
	class  *redactClass
	min    int
	exact  bool
	// noDash: a dash before the prefix is not a start either. The bare
	// "sk-" prefix is three characters, short enough to occur by chance
	// inside dash-joined base64url text.
	noDash bool
}

// Group B. Each must start where the previous byte is not a letter, a digit
// or an underscore. The bounds are the vendors' published formats where one
// is published; docs/claude-wake.md says which.
var redactShapes = []redactShape{
	{name: "github-token", prefix: "ghp_", class: redactW, min: 36, exact: true},
	{name: "github-token", prefix: "gho_", class: redactW, min: 36, exact: true},
	{name: "github-token", prefix: "ghu_", class: redactW, min: 36, exact: true},
	{name: "github-token", prefix: "ghs_", class: redactW, min: 36, exact: true},
	{name: "github-token", prefix: "ghr_", class: redactW, min: 36, exact: true},
	{name: "github-token", prefix: "github_pat_", class: redactWU, min: 82, exact: true},
	{name: "anthropic-key", prefix: "sk-ant-", class: redactWUD, min: 80},
	{name: "openai-key", prefix: "sk-proj-", class: redactWUD, min: 40},
	{name: "openai-key", prefix: "sk-", class: redactW, min: 40, noDash: true},
	{name: "aws-access-key", prefix: "AKIA", class: redactUpper, min: 16, exact: true},
	{name: "aws-access-key", prefix: "ASIA", class: redactUpper, min: 16, exact: true},
	{name: "slack-token", prefix: "xoxa-", class: redactWD, min: 10},
	{name: "slack-token", prefix: "xoxb-", class: redactWD, min: 10},
	{name: "slack-token", prefix: "xoxp-", class: redactWD, min: 10},
	{name: "slack-token", prefix: "xoxr-", class: redactWD, min: 10},
	{name: "slack-token", prefix: "xoxs-", class: redactWD, min: 10},
	{name: "tailscale-key", prefix: "tskey-auth-", class: redactWD, min: 20},
	{name: "tailscale-key", prefix: "tskey-api-", class: redactWD, min: 20},
	{name: "tailscale-key", prefix: "tskey-client-", class: redactWD, min: 20},
	{name: "stripe-key", prefix: "sk_live_", class: redactW, min: 24},
	{name: "stripe-key", prefix: "rk_live_", class: redactW, min: 24},
	{name: "npm-token", prefix: "npm_", class: redactW, min: 36, exact: true},
}

// redactGroupBNames lists the names group B can report, private keys included.
func redactGroupBNames() []string {
	seen := map[string]bool{"private-key": true}
	for _, shape := range redactShapes {
		seen[shape.name] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func redactRun(s string, at int, class *redactClass) int {
	n := 0
	for at+n < len(s) && class[s[at+n]] {
		n++
	}
	return n
}

func redactBoundary(s string, at int) bool {
	return at == 0 || !redactWU[s[at-1]]
}

// redactScan finds what group A and group B match in one string value, and
// what group C matches outside those. Overlapping matches are merged into one
// span named after the leftmost. It reports false when the deadline passed
// before it finished; the spans are then incomplete and must not be used.
func redactScan(s string, exact []string, deadline time.Time) (rewrite, counted []redactSpan, ok bool) {
	late := func() bool { return !time.Now().Before(deadline) }
	// Group A: values this host holds.
	for _, value := range exact {
		for at := 0; ; {
			i := strings.Index(s[at:], value)
			if i < 0 {
				break
			}
			rewrite = append(rewrite, redactSpan{at + i, at + i + len(value), "host-credential"})
			at += i + len(value)
		}
	}
	if late() {
		return nil, nil, false
	}
	// Group B: private keys, from the header line through the matching end
	// line, or to the end of the value when there is none.
	const begin = "-----BEGIN "
	for at := 0; ; {
		i := strings.Index(s[at:], begin)
		if i < 0 {
			break
		}
		start := at + i
		label := start + len(begin)
		n := redactRun(s, label, redactHeader)
		at = label
		if n > 40+len("PRIVATE KEY") || !strings.HasSuffix(s[label:label+n], "PRIVATE KEY") || !strings.HasPrefix(s[label+n:], "-----") {
			continue
		}
		end := len(s)
		closing := "-----END " + s[label:label+n] + "-----"
		if j := strings.Index(s[label+n:], closing); j >= 0 {
			end = label + n + j + len(closing)
		}
		rewrite = append(rewrite, redactSpan{start, end, "private-key"})
		at = end
	}
	// Group B: prefixed tokens.
	for _, shape := range redactShapes {
		if late() {
			return nil, nil, false
		}
		for at := 0; ; {
			i := strings.Index(s[at:], shape.prefix)
			if i < 0 {
				break
			}
			start := at + i
			body := start + len(shape.prefix)
			n := redactRun(s, body, shape.class)
			at = body
			if !redactBoundary(s, start) || shape.noDash && start > 0 && s[start-1] == '-' || n < shape.min || shape.exact && n != shape.min {
				continue
			}
			rewrite = append(rewrite, redactSpan{start, body + n, shape.name})
			at = body + n
		}
	}
	rewrite = redactMerge(rewrite)
	if late() {
		return nil, nil, false
	}
	// Group C: counted, never rewritten.
	for _, span := range redactMerge(redactContext(s)) {
		i := sort.Search(len(rewrite), func(i int) bool { return rewrite[i].end > span.start })
		if i == len(rewrite) || rewrite[i].start >= span.end {
			counted = append(counted, span)
		}
	}
	return rewrite, counted, !late()
}

// redactMerge sorts spans and joins the ones that overlap.
func redactMerge(spans []redactSpan) []redactSpan {
	if len(spans) < 2 {
		return spans
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	merged := spans[:1]
	for _, span := range spans[1:] {
		last := &merged[len(merged)-1]
		if span.start < last.end {
			if span.end > last.end {
				last.end = span.end
			}
			continue
		}
		merged = append(merged, span)
	}
	return merged
}

// redactAssigned parses `= value` or `: value` after a name ending at `at`,
// allowing quotes and spaces, and returns the value's bounds.
func redactAssigned(s string, at int, class *redactClass) (start, end int, ok bool) {
	skip := func(chars string) {
		for at < len(s) && strings.IndexByte(chars, s[at]) >= 0 {
			at++
		}
	}
	skip(`"'`)
	skip(" \t")
	if at >= len(s) || s[at] != '=' && s[at] != ':' {
		return 0, 0, false
	}
	at++
	skip(" \t")
	skip(`"'`)
	start = at
	for at < len(s) && (class == nil && s[at] > ' ' && s[at] != '"' && s[at] != '\'' && s[at] != 0x7f || class != nil && class[s[at]]) {
		at++
	}
	return start, at, true
}

// redactLower lowers ASCII letters only, so every offset stays the same.
func redactLower(s string) string {
	if strings.IndexFunc(s, func(r rune) bool { return r >= 'A' && r <= 'Z' }) < 0 {
		return s
	}
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// redactContext finds the context-keyed shapes of group C.
func redactContext(s string) []redactSpan {
	var spans []redactSpan
	lower := redactLower(s)
	each := func(needle string, found func(at int)) {
		for at := 0; ; {
			i := strings.Index(lower[at:], needle)
			if i < 0 {
				return
			}
			found(at + i)
			at += i + len(needle)
		}
	}
	// assigned-secret: 20 or more varied characters after a secret's name.
	for _, name := range []string{"api_key", "api-key", "apikey", "secret", "token", "password"} {
		each(name, func(at int) {
			start, end, ok := redactAssigned(s, at+len(name), nil)
			if !ok || end-start < 20 {
				return
			}
			distinct := map[byte]bool{}
			for i := start; i < end; i++ {
				distinct[s[i]] = true
			}
			if len(distinct) >= 8 {
				spans = append(spans, redactSpan{start, end, "assigned-secret"})
			}
		})
	}
	// aws-secret-key: exactly 40 characters after the AWS secret key name.
	each("aws_secret_access_key", func(at int) {
		if start, end, ok := redactAssigned(s, at+len("aws_secret_access_key"), redactBase64); ok && end-start == 40 {
			spans = append(spans, redactSpan{start, end, "aws-secret-key"})
		}
	})
	// bearer: the value after Authorization: Bearer.
	each("authorization: bearer ", func(at int) {
		start := at + len("authorization: bearer ")
		end := start
		for end < len(s) && s[end] > ' ' && s[end] != '"' && s[end] != '\'' && s[end] != 0x7f {
			end++
		}
		if end-start >= 8 {
			spans = append(spans, redactSpan{start, end, "bearer"})
		}
	})
	// jwt: three base64url segments whose first two decode as JSON objects.
	// An encoded object starts with `{"`, which encodes as eyJ.
	each("eyj", func(at int) {
		if s[at:at+3] != "eyJ" || !redactBoundary(s, at) {
			return
		}
		end, dots := at, 0
		for end < len(s) && (redactWUD[s[end]] || s[end] == '.' && dots < 2) {
			if s[end] == '.' {
				dots++
			}
			end++
		}
		parts := strings.Split(s[at:end], ".")
		if len(parts) != 3 || parts[2] == "" || end-at > toolRedactLongest*4 {
			return
		}
		for _, part := range parts[:2] {
			raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(part, "="))
			var object map[string]json.RawMessage
			if err != nil || json.Unmarshal(raw, &object) != nil {
				return
			}
		}
		spans = append(spans, redactSpan{at, end, "jwt"})
	})
	// google-api-key and discord-token have no vendor-published bounds, so
	// they are counted here and not rewritten.
	each("aiza", func(at int) {
		if s[at:at+4] == "AIza" && redactBoundary(s, at) && redactRun(s, at+4, redactWUD) == 35 {
			spans = append(spans, redactSpan{at, at + 39, "google-api-key"})
		}
	})
	for at := 0; at < len(s); at++ {
		i := strings.IndexByte(s[at:], '.')
		if i < 0 {
			break
		}
		at += i
		// The middle part is exactly six characters between two dots.
		if redactRun(s, at+1, redactWUD) != 6 || at+7 >= len(s) || s[at+7] != '.' {
			continue
		}
		last := redactRun(s, at+8, redactWUD)
		first := 0
		for first < at && redactWUD[s[at-1-first]] {
			first++
		}
		start := at - first
		if last < 27 || last > 38 || first < 24 || first > 26 || !redactBoundary(s, start) || strings.IndexByte("MNO", s[start]) < 0 {
			continue
		}
		spans = append(spans, redactSpan{start, at + 8 + last, "discord-token"})
		at += 7 + last
	}
	// url-password: the password in scheme://user:password@host.
	each("://", func(at int) {
		colon, end := -1, at+3
		for ; end < len(s) && s[end] > ' ' && s[end] != '/' && s[end] != '@' && s[end] != '"' && s[end] != '\''; end++ {
			if s[end] == ':' && colon < 0 {
				colon = end
			}
		}
		if colon > at+3 && end < len(s) && s[end] == '@' && end > colon+1 {
			spans = append(spans, redactSpan{colon + 1, end, "url-password"})
		}
	})
	return spans
}

// redactResult is what one scan of a tool's output found.
type redactResult struct {
	kinds   map[string]int // matches per pattern name, groups A, B and C
	rewrite int            // matches in groups A and B, the ones redact mode replaces
	timeout bool
}

func (r redactResult) count() int {
	n := 0
	for _, c := range r.kinds {
		n += c
	}
	return n
}

// redactCountValue scans every string value inside a decoded tool output.
// Object keys are structure, not output, and are not scanned.
func redactCountValue(v any, exact []string, deadline time.Time) redactResult {
	r := redactResult{kinds: map[string]int{}}
	var walk func(v any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case string:
			rewrite, counted, ok := redactScan(x, exact, deadline)
			if !ok {
				return false
			}
			r.rewrite += len(rewrite)
			for _, span := range append(rewrite, counted...) {
				r.kinds[span.name]++
			}
		case []any:
			for _, item := range x {
				if !walk(item) {
					return false
				}
			}
		case map[string]any:
			for _, item := range x {
				if !walk(item) {
					return false
				}
			}
		}
		return true
	}
	r.timeout = !walk(v)
	return r
}

// redactRewriteValue returns a copy of a decoded tool output in which each
// group A and group B match is replaced by its placeholder. Only text inside
// string values changes: every key, type, number and position is kept. It
// reports false when the deadline passed first.
func redactRewriteValue(v any, exact []string, deadline time.Time) (any, bool) {
	switch x := v.(type) {
	case string:
		spans, _, ok := redactScan(x, exact, deadline)
		if !ok {
			return nil, false
		}
		if len(spans) == 0 {
			return x, true
		}
		var b strings.Builder
		at := 0
		for _, span := range spans {
			b.WriteString(x[at:span.start])
			b.WriteString("[tt-redacted:" + span.name + "]")
			at = span.end
		}
		b.WriteString(x[at:])
		return b.String(), true
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			var ok bool
			if out[i], ok = redactRewriteValue(item, exact, deadline); !ok {
				return nil, false
			}
		}
		return out, true
	case map[string]any:
		out := make(map[string]any, len(x))
		for key, item := range x {
			var ok bool
			if out[key], ok = redactRewriteValue(item, exact, deadline); !ok {
				return nil, false
			}
		}
		return out, true
	}
	return v, true
}

// redactSameShape reports whether two decoded values differ only in the text
// of their strings.
func redactSameShape(a, b any) bool {
	switch x := a.(type) {
	case string:
		_, ok := b.(string)
		return ok
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !redactSameShape(x[i], y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for key := range x {
			other, ok := y[key]
			if !ok || !redactSameShape(x[key], other) {
				return false
			}
		}
		return true
	}
	return a == b
}

// redact is the PostToolUse step. It fills the row's redaction fields and, in
// redact mode with something to replace and time left, writes Claude Code the
// one JSON object carrying the rewritten output. Every other path writes
// nothing, so the original output passes.
func (l toolLedger) redact(row *toolLedgerRow, data []byte) {
	row.RedactMode = l.redactMode
	skip := func(reason string) { row.RedactSkip = reason }
	var in struct {
		Response json.RawMessage `json:"tool_response"`
	}
	if json.Unmarshal(data, &in) != nil || len(bytes.TrimSpace(in.Response)) == 0 {
		skip("unreadable")
		return
	}
	if len(in.Response) > l.redactMax {
		skip("oversize")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(in.Response))
	dec.UseNumber()
	var output any
	if dec.Decode(&output) != nil {
		skip("unreadable")
		return
	}
	deadline := l.started.Add(l.redactBudget)
	exact := toolRedactExact(l.token)
	found := redactCountValue(output, exact, deadline)
	if found.timeout {
		skip("timeout")
		return
	}
	count := found.count()
	row.RedactCount = &count
	if count > 0 {
		row.RedactKinds = found.kinds
	}
	if l.redactMode != toolRedactRedact || found.rewrite == 0 {
		return
	}
	updated, ok := toolRedactRewrite(output, exact, deadline)
	if !ok {
		skip("timeout")
		return
	}
	if !redactSameShape(output, updated) {
		skip("shape")
		return
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	err := enc.Encode(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":     "PostToolUse",
		"updatedToolOutput": updated,
		"additionalContext": redactNotice(found.rewrite),
	}})
	if err != nil {
		skip("shape")
		return
	}
	if !time.Now().Before(deadline) {
		skip("timeout")
		return
	}
	// The answer goes out before the row: protecting the output matters more
	// than recording it. A blocked write ends with the handler's deadline.
	_, _ = l.stdout.Write(out.Bytes())
}

func redactNotice(n int) string {
	values := "1 value"
	if n != 1 {
		values = strconv.Itoa(n) + " values"
	}
	return "Tailterm replaced " + values + " in this tool output with a [tt-redacted:…] placeholder because it looked like a token or key. The real value is unchanged where it lives. Do not write a placeholder into any file: an edit or a whole-file write that includes it would replace the real value."
}
