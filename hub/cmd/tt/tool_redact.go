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
	"unicode/utf16"
	"unicode/utf8"
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
	// Largest tool_response scanned, in bytes. It is set by measurement: the
	// Mini, under load from other work, scans the slowest text of this size
	// inside 50 ms, half the scan budget. TestToolRedactBenchmark measures it.
	toolRedactMaxBytes = 512 << 10
	// toolRedactRewrite builds the redacted output. Only redact mode calls it.
	toolRedactRewrite = redactRewriteRaw
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

// redactRunMax counts the class bytes from at, stopping once past max.
func redactRunMax(s string, at int, class *redactClass, max int) int {
	n := 0
	for at+n < len(s) && n <= max && class[s[at+n]] {
		n++
	}
	return n
}

func redactBoundary(s string, at int) bool {
	return at == 0 || !redactWU[s[at-1]]
}

// redactClock tells a scan when its deadline has passed. Every loop in a
// scan takes a step per turn. In a short string the time is read once every
// 256 steps; in a long one, where a single step can search the whole string,
// once every 16. So no loop runs on for long after the deadline.
type redactClock struct {
	deadline time.Time
	steps    int
	mask     int
	expired  bool
}

func newRedactClock(deadline time.Time, size int) *redactClock {
	if size > 4<<10 {
		return &redactClock{deadline: deadline, mask: 15}
	}
	return &redactClock{deadline: deadline, mask: 255}
}

func (c *redactClock) late() bool {
	if c.expired {
		return true
	}
	if c.steps++; c.steps&c.mask != 0 {
		return false
	}
	return c.now()
}

func (c *redactClock) now() bool {
	c.expired = c.expired || !time.Now().Before(c.deadline)
	return c.expired
}

// redactScan finds what group A and group B match in one string value, and
// what group C matches outside those. Overlapping matches are merged into one
// span named after the leftmost. It reports false when the deadline passed
// before it finished; the spans are then incomplete and must not be used.
//
// Every loop is linear in the string: a run of token characters is measured
// once and remembered for the later prefixes inside it, and a value that
// could be long is measured only as far as its bound.
func redactScan(s string, exact []string, deadline time.Time) (rewrite, counted []redactSpan, ok bool) {
	clock := newRedactClock(deadline, len(s))
	// Group A: values this host holds.
	for _, value := range exact {
		for at := 0; ; {
			i := strings.Index(s[at:], value)
			if i < 0 {
				break
			}
			rewrite = append(rewrite, redactSpan{at + i, at + i + len(value), "host-credential"})
			at += i + len(value)
			if clock.late() {
				return nil, nil, false
			}
		}
	}
	// Group B: private keys, from the header line through the matching end
	// line, or to the end of the value when there is none.
	const begin = "-----BEGIN "
	for at := 0; ; {
		i := strings.Index(s[at:], begin)
		if i < 0 {
			break
		}
		if clock.late() {
			return nil, nil, false
		}
		start := at + i
		label := start + len(begin)
		n := redactRunMax(s, label, redactHeader, 40+len("PRIVATE KEY"))
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
		if clock.late() {
			return nil, nil, false
		}
		// runEnd is where the last measured run of this shape's class ends.
		// A later prefix whose body starts inside it ends at the same place.
		runEnd := 0
		for at := 0; ; {
			i := strings.Index(s[at:], shape.prefix)
			if i < 0 {
				break
			}
			if clock.late() {
				return nil, nil, false
			}
			start := at + i
			body := start + len(shape.prefix)
			if body >= runEnd {
				runEnd = body
				for runEnd < len(s) && shape.class[s[runEnd]] {
					runEnd++
				}
			}
			n := runEnd - body
			at = body
			if !redactBoundary(s, start) || shape.noDash && start > 0 && s[start-1] == '-' || n < shape.min || shape.exact && n != shape.min {
				continue
			}
			rewrite = append(rewrite, redactSpan{start, body + n, shape.name})
			at = body + n
		}
	}
	rewrite = redactMerge(rewrite)
	// Group C: counted, never rewritten.
	context, ok := redactContext(s, clock)
	if !ok {
		return nil, nil, false
	}
	for _, span := range redactMerge(context) {
		i := sort.Search(len(rewrite), func(i int) bool { return rewrite[i].end > span.start })
		if i == len(rewrite) || rewrite[i].start >= span.end {
			counted = append(counted, span)
		}
	}
	return rewrite, counted, !clock.now()
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

// redactValueByte reports whether a byte can be part of an unquoted value.
func redactValueByte(c byte) bool {
	return c > ' ' && c != '"' && c != '\'' && c != 0x7f
}

// redactAssigned parses `=` or `:` after a name ending at `at`, allowing
// quotes and spaces, and returns where the value starts.
func redactAssigned(s string, at int) (int, bool) {
	skip := func(chars string) {
		for n := 0; at < len(s) && n < 8 && strings.IndexByte(chars, s[at]) >= 0; n++ {
			at++
		}
	}
	skip(`"'`)
	skip(" \t")
	if at >= len(s) || s[at] != '=' && s[at] != ':' {
		return 0, false
	}
	at++
	skip(" \t")
	skip(`"'`)
	return at, true
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

// redactContext finds the context-keyed shapes of group C. It reports false
// when the clock ran out.
func redactContext(s string, clock *redactClock) ([]redactSpan, bool) {
	var spans []redactSpan
	lower := redactLower(s)
	each := func(needle string, found func(at int)) {
		for at := 0; !clock.late(); {
			i := strings.Index(lower[at:], needle)
			if i < 0 {
				return
			}
			found(at + i)
			at += i + len(needle)
		}
	}
	// assigned-secret: 20 or more characters, varied within the first 64,
	// after a secret's name. valueEnd remembers where the last measured
	// value ends: a later name inside it has a value ending at the same place.
	valueEnd := 0
	for _, name := range []string{"api_key", "api-key", "apikey", "secret", "token", "password"} {
		valueEnd = 0
		each(name, func(at int) {
			start, ok := redactAssigned(s, at+len(name))
			if !ok {
				return
			}
			if start >= valueEnd {
				valueEnd = start
				for valueEnd < len(s) && redactValueByte(s[valueEnd]) {
					valueEnd++
				}
			}
			if valueEnd-start < 20 {
				return
			}
			var seen [256]bool
			distinct := 0
			for i := start; i < valueEnd && i < start+64 && distinct < 8; i++ {
				if !seen[s[i]] {
					seen[s[i]], distinct = true, distinct+1
				}
			}
			if distinct >= 8 {
				spans = append(spans, redactSpan{start, valueEnd, "assigned-secret"})
			}
		})
	}
	// aws-secret-key: exactly 40 characters after the AWS secret key name.
	each("aws_secret_access_key", func(at int) {
		if start, ok := redactAssigned(s, at+len("aws_secret_access_key")); ok && redactRunMax(s, start, redactBase64, 40) == 40 {
			spans = append(spans, redactSpan{start, start + 40, "aws-secret-key"})
		}
	})
	// bearer: the value after Authorization: Bearer. The name holds a space
	// and a value does not, so one value is never measured twice.
	each("authorization: bearer ", func(at int) {
		start := at + len("authorization: bearer ")
		end := start
		for end < len(s) && redactValueByte(s[end]) {
			end++
		}
		if end-start >= 8 {
			spans = append(spans, redactSpan{start, end, "bearer"})
		}
	})
	// jwt: three base64url segments whose first two decode as JSON objects.
	// An encoded object starts with `{"`, which encodes as eyJ. A candidate
	// that starts inside the one before it is not looked at again.
	jwtEnd := 0
	each("eyj", func(at int) {
		if at < jwtEnd || s[at:at+3] != "eyJ" || !redactBoundary(s, at) {
			return
		}
		end, dots := at, 0
		var dot [2]int
		for end < len(s) && (redactWUD[s[end]] || s[end] == '.' && dots < 2) {
			if s[end] == '.' {
				dot[dots] = end
				dots++
			}
			end++
		}
		jwtEnd = end
		if dots != 2 || dot[1]+1 == end || end-at > toolRedactLongest*4 {
			return
		}
		for _, part := range []string{s[at:dot[0]], s[dot[0]+1 : dot[1]]} {
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
		if s[at:at+4] == "AIza" && redactBoundary(s, at) && redactRunMax(s, at+4, redactWUD, 35) == 35 {
			spans = append(spans, redactSpan{at, at + 39, "google-api-key"})
		}
	})
	for at := 0; at < len(s) && !clock.late(); at++ {
		i := strings.IndexByte(s[at:], '.')
		if i < 0 {
			break
		}
		at += i
		// The middle part is exactly six characters between two dots.
		if redactRunMax(s, at+1, redactWUD, 6) != 6 || at+7 >= len(s) || s[at+7] != '.' {
			continue
		}
		last := redactRunMax(s, at+8, redactWUD, 38)
		first := 0
		for first < at && first <= 26 && redactWUD[s[at-1-first]] {
			first++
		}
		start := at - first
		if last < 27 || last > 38 || first < 24 || first > 26 || !redactBoundary(s, start) || strings.IndexByte("MNO", s[start]) < 0 {
			continue
		}
		spans = append(spans, redactSpan{start, at + 8 + last, "discord-token"})
		at += 7 + last
	}
	// url-password: the password in scheme://user:password@host. The scan
	// stops at the first slash, so it never passes the next "://".
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
	return spans, !clock.expired
}

// redactEachString calls found with the bounds, quotes included, of every
// string value in a JSON document, in order. Object keys are structure, not
// output, and are skipped. The document is one encoding/json already
// accepted, so this only has to tell keys from values.
func redactEachString(raw []byte, found func(start, end int) bool) bool {
	var objects []bool // one entry per open container: true for an object
	key := false       // the next string is an object key
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '{':
			objects, key = append(objects, true), true
		case '[':
			objects, key = append(objects, false), false
		case '}', ']':
			if len(objects) > 0 {
				objects = objects[:len(objects)-1]
			}
			key = false
		case ',':
			key = len(objects) > 0 && objects[len(objects)-1]
		case ':':
			key = false
		case '"':
			end := i + 1
			for end < len(raw) && raw[end] != '"' {
				if raw[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(raw) {
				return false
			}
			if !key && !found(i, end+1) {
				return false
			}
			i = end
		}
	}
	return true
}

// redactDecode returns the text of one JSON string token. With no escape in
// the token, the text is the bytes between the quotes and offsets is nil.
// Otherwise offsets[i] is where, in the token, the escape or byte that
// produced decoded byte i begins, with one more entry for the closing quote,
// so a match found in the text can be cut out of the token and every other
// byte of it left exactly as it was written.
func redactDecode(token []byte) (text string, offsets []int32) {
	body := token[1 : len(token)-1]
	if bytes.IndexByte(body, '\\') < 0 {
		return string(body), nil
	}
	out := make([]byte, 0, len(body))
	offsets = make([]int32, 0, len(body)+1)
	put := func(at int, b ...byte) {
		for range b {
			offsets = append(offsets, int32(at))
		}
		out = append(out, b...)
	}
	hex := func(at int) (rune, bool) {
		if at+6 > len(token)-1 || token[at] != '\\' || token[at+1] != 'u' {
			return 0, false
		}
		n, err := strconv.ParseUint(string(token[at+2:at+6]), 16, 16)
		return rune(n), err == nil
	}
	for i := 1; i < len(token)-1; {
		if token[i] != '\\' {
			put(i, token[i])
			i++
			continue
		}
		switch c := token[i+1]; c {
		case 'u':
			r, _ := hex(i)
			size := 6
			if utf16.IsSurrogate(r) {
				// A pair is one character; a lone half has no text form.
				if low, ok := hex(i + 6); ok && utf16.IsSurrogate(low) && utf16.DecodeRune(r, low) != utf8.RuneError {
					r, size = utf16.DecodeRune(r, low), 12
				} else {
					r = utf8.RuneError
				}
			}
			put(i, []byte(string(r))...)
			i += size
		case 'b':
			put(i, '\b')
			i += 2
		case 'f':
			put(i, '\f')
			i += 2
		case 'n':
			put(i, '\n')
			i += 2
		case 'r':
			put(i, '\r')
			i += 2
		case 't':
			put(i, '\t')
			i += 2
		default: // a quote, a backslash or a slash
			put(i, c)
			i += 2
		}
	}
	offsets = append(offsets, int32(len(token)-1))
	return string(out), offsets
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

// redactCountRaw scans every string value of a tool's output and counts the
// matches. It changes nothing and builds no output.
func redactCountRaw(raw []byte, exact []string, deadline time.Time) redactResult {
	r := redactResult{kinds: map[string]int{}}
	r.timeout = !redactEachString(raw, func(start, end int) bool {
		text, _ := redactDecode(raw[start:end])
		rewrite, counted, ok := redactScan(text, exact, deadline)
		if !ok {
			return false
		}
		r.rewrite += len(rewrite)
		for _, span := range rewrite {
			r.kinds[span.name]++
		}
		for _, span := range counted {
			r.kinds[span.name]++
		}
		return true
	})
	return r
}

// redactRewriteRaw returns a tool's output with each group A and group B
// match cut out of its string and the placeholder put in its place. Every
// other byte is copied as it was written: structure, numbers, spacing,
// escapes and the text around a match. It reports false when the deadline
// passed first.
func redactRewriteRaw(raw []byte, exact []string, deadline time.Time) ([]byte, bool) {
	out := make([]byte, 0, len(raw))
	copied := 0
	ok := redactEachString(raw, func(start, end int) bool {
		token := raw[start:end]
		text, offsets := redactDecode(token)
		spans, _, ok := redactScan(text, exact, deadline)
		if !ok {
			return false
		}
		for _, span := range spans {
			from, to := span.start+1, span.end+1
			if offsets != nil {
				// Step past the other bytes of a character the match ends in.
				for span.end < len(text) && span.end > 0 && offsets[span.end] == offsets[span.end-1] {
					span.end++
				}
				from, to = int(offsets[span.start]), int(offsets[span.end])
			}
			out = append(out, raw[copied:start+from]...)
			out = append(out, "[tt-redacted:"+span.name+"]"...)
			copied = start + to
		}
		return true
	})
	if !ok {
		return nil, false
	}
	return append(out, raw[copied:]...), true
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

// redactSameShapeRaw decodes both documents and compares their shapes.
func redactSameShapeRaw(a, b []byte) bool {
	decode := func(raw []byte) (any, bool) {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var v any
		return v, dec.Decode(&v) == nil && !dec.More()
	}
	x, okX := decode(a)
	y, okY := decode(b)
	return okX && okY && redactSameShape(x, y)
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
	deadline := l.started.Add(l.redactBudget)
	if !time.Now().Before(deadline) {
		skip("timeout") // reading the input used the whole budget
		return
	}
	exact := toolRedactExact(l.token)
	found := redactCountRaw(in.Response, exact, deadline)
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
	updated, ok := toolRedactRewrite(in.Response, exact, deadline)
	if !ok {
		skip("timeout")
		return
	}
	if !redactSameShapeRaw(in.Response, updated) {
		skip("shape")
		return
	}
	notice, err := json.Marshal(redactNotice(found.rewrite))
	if err != nil {
		skip("shape")
		return
	}
	// The answer is assembled by hand so the output's own bytes are not
	// encoded a second time.
	var out bytes.Buffer
	out.Grow(len(updated) + len(notice) + 128)
	out.WriteString(`{"hookSpecificOutput":{"hookEventName":"PostToolUse","updatedToolOutput":`)
	out.Write(updated)
	out.WriteString(`,"additionalContext":`)
	out.Write(notice)
	out.WriteString("}}\n")
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
