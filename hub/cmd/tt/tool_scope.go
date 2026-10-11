package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

// The owned-paths report (feature wi_c1ef66c9f6c8aadc) is report-only. When
// `tt inbox` fetches a typed ASSIGN addressed to its agent, it keeps that
// message's owned paths in a private file beside the agent's ledger. On
// PreToolUse the tool-call ledger reads that file and appends one
// would_report row for an Edit, Write, MultiEdit or NotebookEdit whose file
// is inside the session's repository and outside those paths.
//
// The edit-before-assign report (feature wi_03fe150782cc7a8e) is report-only
// too. After a successful fetch of its agent's own messages `tt inbox` leaves
// a marker, inbox.json, beside that cache, once per run. The hook appends one
// would_report row, rule edit-before-assign, for the same four tools when the
// agent has a queue builder's name, the marker is this run's and no owned
// paths are cached for this run.
//
// It blocks nothing and prints nothing, and any failure writes nothing
// extra. The hook reads the cache and the marker and makes stat calls only:
// no process is started and the hub is never asked. See docs/claude-wake.md.

// Variables rather than constants so tests can change them.
var (
	toolScopeCheck    = toolLedger.wouldReport
	toolScopeLockWait = 200 * time.Millisecond // the most a cache write delays tt inbox
	toolScopeHold     = func() {}              // runs under the cache lock, between the read and the rename
	toolScopeSince    = time.Since             // the clock the scope row's own deadline checks read
	toolScopeRecheck  = func() {}              // runs in the hook between the marker read and the second cache read
)

const (
	toolScopeOutcome    = "would_report"
	toolScopeRule       = "outside-owned-paths"
	toolScopeMaxEntries = 256       // owned paths one ASSIGN may cache
	toolScopeEntryBytes = 512       // largest cached entry
	toolScopeCacheBytes = 192 << 10 // largest cache file the hook reads
	toolScopePathBytes  = 200       // largest stored path
	toolScopeMaxLevels  = 64        // directories walked up to find the repository root
	toolScopeNote       = " (new)"  // the one note an entry may end with
	toolScopeCacheName  = "owns.json"

	toolScopeBeforeRule = "edit-before-assign"
	toolScopeMarkName   = "inbox.json"
	toolScopeMarkBytes  = 512 // largest marker file the hook reads
)

var errToolScopeLate = errors.New("the hook deadline passed")

// toolScopeCache is owns.json. It holds no message text: only the paths, the
// ASSIGN's number and its work item.
type toolScopeCache struct {
	V            int      `json:"v"`
	Run          string   `json:"run"` // the run of the tt inbox that wrote it
	AssignSeq    int64    `json:"assignSeq"`
	Item         string   `json:"item"`
	ItemRevision int64    `json:"itemRevision"`
	Time         string   `json:"time"`
	Owns         []string `json:"owns,omitempty"`
	// An ASSIGN past a bound keeps no paths; the hook then reports nothing.
	Overflow bool `json:"overflow,omitempty"`
}

// toolScopeMark is inbox.json: this run of the agent has fetched its own
// messages. It holds nothing else.
type toolScopeMark struct {
	V    int    `json:"v"`
	Run  string `json:"run"` // the run of the tt inbox that wrote it
	Time string `json:"time"`
}

// toolScopeRemember caches the owned paths of the newest typed ASSIGN among
// msgs that is addressed to this agent, and writes no marker.
func toolScopeRemember(e env, msgs []api.Message) { toolScopeInbox(e, msgs, false) }

// toolScopeInbox caches the owned paths of the newest typed ASSIGN among msgs
// that is addressed to this agent and, when fetchOK says a fetch of the
// agent's own messages succeeded, leaves this run's marker once. It returns
// nothing and prints nothing, so it cannot change what tt inbox prints or
// returns.
func toolScopeInbox(e env, msgs []api.Message, fetchOK bool) {
	defer func() { _ = recover() }()
	root := toolLedgerRoot()
	if root == "" || !toolLedgerAgentOK(e.agent) {
		return
	}
	var assign *api.Message
	for i := range msgs {
		m := &msgs[i]
		if m.To != e.agent || m.Envelope == nil || m.Envelope.Kind != api.EnvelopeKindAssign || len(m.Envelope.Body.Owns) == 0 {
			continue
		}
		if assign == nil || m.Seq > assign.Seq {
			assign = m
		}
	}
	dir, run := filepath.Join(root, e.agent), toolLedgerCut(e.runID)
	// The marker is written once per run: a later read finds it here, without
	// the lock, and with no ASSIGN to cache takes no lock at all.
	needMark := fetchOK && run != ""
	if needMark {
		if mark, ok := toolScopeMarkLoad(dir); ok && mark.Run == run {
			needMark = false
		}
	}
	var next toolScopeCache
	var data []byte
	if assign != nil {
		next = toolScopeCache{V: 1, Run: run, AssignSeq: assign.Seq, Time: toolLedgerNow().UTC().Format(time.RFC3339)}
		for i, link := range assign.WorkItems {
			if i == 0 || link.Relationship == "primary" {
				next.Item, next.ItemRevision = toolLedgerCut(link.ItemID), link.ItemRevision
			}
			if link.Relationship == "primary" {
				break
			}
		}
		next.Owns = assign.Envelope.Body.Owns
		if len(next.Owns) > toolScopeMaxEntries {
			next.Owns, next.Overflow = nil, true
		}
		for _, entry := range next.Owns {
			if len(entry) > toolScopeEntryBytes {
				next.Owns, next.Overflow = nil, true
				break
			}
		}
		var err error
		data, err = json.Marshal(next)
		if err == nil && len(data) > toolScopeCacheBytes {
			// Escaping grew the file past what the hook reads.
			next.Owns, next.Overflow = nil, true
			data, err = json.Marshal(next)
		}
		if err != nil {
			return
		}
	}
	if assign == nil && !needMark {
		return
	}
	if os.MkdirAll(dir, 0700) != nil {
		return
	}
	// The read, the comparison and the rename happen under one lock, so two
	// tt inbox processes cannot leave the older ASSIGN in place. Without the
	// lock nothing is written; a later read of the ASSIGN caches it. The
	// marker is written under the same hold of the lock, so the two files
	// together wait for it once.
	unlock := toolScopeLock(dir)
	if unlock == nil {
		return
	}
	defer unlock()
	if assign != nil {
		if old, ok := toolScopeLoad(dir); !ok || old.Run != next.Run || old.AssignSeq < next.AssignSeq {
			toolScopeHold()
			if !toolScopeWrite(dir, "owns-*.tmp", toolScopeCacheName, data) {
				return
			}
		}
	}
	// The cache is renamed first: the hook never sees this run's marker while
	// the ASSIGN the same read fetched is still uncached.
	if !needMark {
		return
	}
	if mark, ok := toolScopeMarkLoad(dir); ok && mark.Run == run {
		return
	}
	if data, err := json.Marshal(toolScopeMark{V: 1, Run: run, Time: toolLedgerNow().UTC().Format(time.RFC3339)}); err == nil {
		toolScopeWrite(dir, "inbox-*.tmp", toolScopeMarkName, data)
	}
}

// toolScopeWrite puts data at name in dir through a temp file and a rename,
// and reports whether it did. A failure leaves no temp file.
func toolScopeWrite(dir, pattern, name string, data []byte) bool {
	f, err := os.CreateTemp(dir, pattern) // created 0600
	if err != nil {
		return false
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, name))
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err == nil
}

// toolScopeLock takes the cache lock the way toolLedger.lock takes the
// ledger's. It returns the unlock function, or nil when the lock was not
// obtained within the wait. The lock ends with the process.
func toolScopeLock(dir string) func() {
	f, err := os.OpenFile(filepath.Join(dir, "owns.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil
	}
	wait := toolScopeLockWait
	for waited := time.Duration(0); ; waited += toolLedgerLockRetry {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }
		}
		if (!errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN)) || waited >= wait {
			_ = f.Close()
			return nil
		}
		time.Sleep(toolLedgerLockRetry)
	}
}

// toolScopeLoad reads owns.json. A missing, unreadable, oversized or
// malformed file, or one of another version, is no cache.
func toolScopeLoad(dir string) (toolScopeCache, bool) {
	var cache toolScopeCache
	f, err := os.Open(filepath.Join(dir, toolScopeCacheName))
	if err != nil {
		return cache, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, toolScopeCacheBytes+1))
	if err != nil || len(data) > toolScopeCacheBytes || json.Unmarshal(data, &cache) != nil || cache.V != 1 {
		return toolScopeCache{}, false
	}
	return cache, true
}

// toolScopeMarkLoad reads inbox.json. A missing, unreadable, oversized or
// malformed file, or one of another version, is no marker.
func toolScopeMarkLoad(dir string) (toolScopeMark, bool) {
	var mark toolScopeMark
	f, err := os.Open(filepath.Join(dir, toolScopeMarkName))
	if err != nil {
		return mark, false
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, toolScopeMarkBytes+1))
	if err != nil || len(data) > toolScopeMarkBytes || json.Unmarshal(data, &mark) != nil || mark.V != 1 {
		return toolScopeMark{}, false
	}
	return mark, true
}

// toolScopePathField names where each edit tool's input carries its file.
var toolScopePathField = map[string]string{
	"Edit": "file_path", "Write": "file_path", "MultiEdit": "file_path", "NotebookEdit": "notebook_path",
}

// wouldReport appends the would_report row of a pre whose file is inside the
// repository and outside the cached owned paths. The steps are ordered so a
// call that is not an edit, or has no cache, costs next to nothing. With no
// cache of this run it is beforeAssign that decides.
func (l toolLedger) wouldReport(row toolLedgerRow, tool string, input json.RawMessage, cwd string) {
	defer func() { _ = recover() }()
	field := toolScopePathField[tool]
	if field == "" {
		return
	}
	cache, ok := toolScopeLoad(l.dir)
	if !ok || cache.Run != l.run {
		l.beforeAssign(row, input, field, cwd, ok)
		return
	}
	if cache.Overflow || len(cache.Owns) == 0 {
		return
	}
	root, rel, ok := toolScopeFile(input, field, cwd)
	if !ok {
		return
	}
	exists := func(form string) bool {
		_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(form)))
		return err == nil
	}
	if owned, usable := toolScopeOwned(root, rel, cache.Owns, exists); owned || !usable {
		return
	}
	l.scopeRow(row, toolScopeRule, cache.AssignSeq, rel)
}

// toolScopeBuilder reports whether name has the queue team builder form:
// builder- and eight lower-case hex digits.
func toolScopeBuilder(name string) bool {
	const prefix = "builder-"
	if len(name) != len(prefix)+8 || name[:len(prefix)] != prefix {
		return false
	}
	for i := len(prefix); i < len(name); i++ {
		if c := name[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// beforeAssign appends the edit-before-assign row of a pre by a queue builder
// whose run has fetched its inbox and has no owned paths cached. loaded says
// owns.json was read, and so is another run's. The steps are ordered so any
// other agent touches no file.
func (l toolLedger) beforeAssign(row toolLedgerRow, input json.RawMessage, field, cwd string, loaded bool) {
	if l.helper || !toolScopeBuilder(l.name) || l.run == "" {
		return
	}
	if !loaded {
		// A cache that is there and cannot be used is not "no ASSIGN cached".
		if _, err := os.Lstat(filepath.Join(l.dir, toolScopeCacheName)); !errors.Is(err, os.ErrNotExist) {
			return
		}
	}
	if mark, ok := toolScopeMarkLoad(l.dir); !ok || mark.Run != l.run {
		return
	}
	// tt inbox renames the cache before the marker, so a second read settles
	// an ASSIGN that landed between the first read and the marker's.
	toolScopeRecheck()
	if cache, ok := toolScopeLoad(l.dir); ok && cache.Run == l.run {
		return
	}
	if _, rel, ok := toolScopeFile(input, field, cwd); ok {
		l.scopeRow(row, toolScopeBeforeRule, 0, rel)
	}
}

// toolScopeFile resolves the file an edit names to the repository root above
// cwd and the file's path relative to it. It gives false for an input without
// its path, a cwd that is not absolute, no repository, or a file outside it.
func toolScopeFile(input json.RawMessage, field, cwd string) (root, rel string, ok bool) {
	file, ok := toolScopeField(input, field)
	if !ok || file == "" || !filepath.IsAbs(cwd) {
		return "", "", false
	}
	cwd = filepath.Clean(cwd)
	if !filepath.IsAbs(file) {
		file = filepath.Join(cwd, file)
	}
	root = toolScopeRoot(cwd)
	if root == "" {
		return "", "", false
	}
	rel, ok = toolScopeRel(root, filepath.Clean(file))
	return root, rel, ok // not ok: outside the repository
}

// scopeRow appends one would_report row of the rule for rel, a path relative
// to the repository root.
func (l toolLedger) scopeRow(row toolLedgerRow, rule string, assignSeq int64, rel string) {
	// The path goes through the secret scan as a would-deny summary does. A
	// scan that did not finish gives no path, and so no row.
	rewrite, counted, ok := redactScan(rel, toolRedactExact(l.token), l.started.Add(l.redactBudget))
	if !ok {
		return
	}
	var out strings.Builder
	at := 0
	for _, span := range redactMerge(append(rewrite, counted...)) {
		out.WriteString(rel[at:span.start])
		out.WriteString("[redacted]")
		at = span.end
	}
	out.WriteString(rel[at:])
	row.Outcome, row.ReportRule, row.AssignSeq = toolScopeOutcome, rule, assignSeq
	if row.ReportPath = toolLedgerCutTo(out.String(), toolScopePathBytes); row.ReportPath == "" {
		return
	}
	// Appended through a copy of the ledger that cannot wait for the lock
	// past the hook's deadline and checks the deadline again immediately
	// before its one write. toolLedger.append itself is unchanged.
	left := l.deadline - toolScopeSince(l.started)
	if left <= 0 {
		return
	}
	s := l
	if s.lockWait > left {
		s.lockWait = left
	}
	write := l.write
	s.write = func(f *os.File, b []byte) error {
		if toolScopeSince(l.started) >= l.deadline {
			return errToolScopeLate
		}
		return write(f, b)
	}
	s.append(row)
}

// toolScopeRoot walks up from cwd to the first directory holding .git, as a
// file or a directory. Stat calls only.
func toolScopeRoot(cwd string) string {
	dir := cwd
	for level := 0; level < toolScopeMaxLevels; level++ {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// toolScopeRel gives file relative to root with forward slashes, and false
// when file is outside root. Both are clean absolute paths; nothing is
// resolved, so a symbolic link is judged by its own name.
func toolScopeRel(root, file string) (string, bool) {
	rel, err := filepath.Rel(root, file)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// toolScopeForm is an entry's literal form: the entry as written with its
// surrounding spaces, a leading ./ and a trailing / dropped, and an absolute
// entry made relative to the root. Brackets, parentheses, * and ? are
// ordinary characters. An empty entry and one outside the root have no form.
func toolScopeForm(root, entry string) (string, bool) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return "", false
	}
	if filepath.IsAbs(entry) {
		return toolScopeRel(root, filepath.Clean(entry))
	}
	form := path.Clean(filepath.ToSlash(entry))
	if form == ".." || strings.HasPrefix(form, "../") {
		return "", false
	}
	return form, true
}

// toolScopeUnder reports whether rel is the form or lies under it. The form
// "." is the whole repository.
func toolScopeUnder(form, rel string) bool {
	return form == "." || rel == form || strings.HasPrefix(rel, form+"/")
}

// toolScopeOwned reports whether rel, a path relative to root, is owned by
// one of the entries, and whether any entry could be judged at all. It makes
// three passes, each over every entry:
//
//  1. the literal form, always first;
//  2. the form without a closing " (new)" note, only when nothing of the
//     noted name exists;
//  3. the form as a glob over the whole path, only when it has a glob
//     character and nothing of that exact name exists.
//
// So an entry that names something existing only ever owns that thing.
func toolScopeOwned(root, rel string, entries []string, exists func(string) bool) (owned, usable bool) {
	forms := make([]string, 0, len(entries))
	for _, entry := range entries {
		if form, ok := toolScopeForm(root, entry); ok {
			forms = append(forms, form)
		}
	}
	if len(forms) == 0 {
		return false, false
	}
	for _, form := range forms {
		if toolScopeUnder(form, rel) {
			return true, true
		}
	}
	for _, form := range forms {
		if !strings.HasSuffix(form, toolScopeNote) || exists(form) {
			continue
		}
		if bare, ok := toolScopeForm(root, strings.TrimSuffix(form, toolScopeNote)); ok && toolScopeUnder(bare, rel) {
			return true, true
		}
	}
	for _, form := range forms {
		if !strings.ContainsAny(form, "*?[") || exists(form) {
			continue
		}
		if matched, err := path.Match(form, rel); err == nil && matched {
			return true, true
		}
	}
	return false, true
}

// toolScopeField returns the string value of a top-level key of a JSON
// object, the last one when the key repeats. raw is tool_input, which the
// hook's decode has already found valid. Other values are skipped without
// being decoded, so megabytes of file content cost one pass over the bytes.
func toolScopeField(raw []byte, key string) (string, bool) {
	at := toolScopeSpace(raw, 0)
	if at >= len(raw) || raw[at] != '{' {
		return "", false
	}
	want := []byte(`"` + key + `"`)
	var value []byte
	for at = toolScopeSpace(raw, at+1); at < len(raw) && raw[at] == '"'; {
		end := toolScopeString(raw, at)
		if end < 0 {
			return "", false
		}
		name := raw[at:end]
		at = toolScopeSpace(raw, end)
		if at >= len(raw) || raw[at] != ':' {
			return "", false
		}
		at = toolScopeSpace(raw, at+1)
		if end = toolScopeValue(raw, at); end < 0 {
			return "", false
		}
		if bytes.Equal(name, want) {
			value = raw[at:end]
		}
		at = toolScopeSpace(raw, end)
		if at < len(raw) && raw[at] == ',' {
			at = toolScopeSpace(raw, at+1)
		}
	}
	var s string
	if len(value) == 0 || value[0] != '"' || json.Unmarshal(value, &s) != nil {
		return "", false
	}
	return s, true
}

func toolScopeSpace(raw []byte, at int) int {
	for at < len(raw) && (raw[at] == ' ' || raw[at] == '\t' || raw[at] == '\n' || raw[at] == '\r') {
		at++
	}
	return at
}

// toolScopeString returns the index after the string starting at at, or -1.
func toolScopeString(raw []byte, at int) int {
	for i := at + 1; i < len(raw); {
		quote := bytes.IndexByte(raw[i:], '"')
		if quote < 0 {
			return -1
		}
		end := i + quote
		slashes := 0
		for j := end - 1; j >= i && raw[j] == '\\'; j-- {
			slashes++
		}
		if slashes%2 == 0 {
			return end + 1
		}
		i = end + 1
	}
	return -1
}

// toolScopeValue returns the index after the JSON value starting at at, or -1.
func toolScopeValue(raw []byte, at int) int {
	if at >= len(raw) {
		return -1
	}
	switch raw[at] {
	case '"':
		return toolScopeString(raw, at)
	case '{', '[':
		for depth := 0; at < len(raw); {
			switch raw[at] {
			case '"':
				if at = toolScopeString(raw, at); at < 0 {
					return -1
				}
				continue
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return at + 1
				}
			}
			at++
		}
		return -1
	}
	for at < len(raw) && !strings.ContainsRune(",}] \t\n\r", rune(raw[at])) {
		at++
	}
	return at
}
