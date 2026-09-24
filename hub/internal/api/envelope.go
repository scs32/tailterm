package api

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Envelope is a typed board message (docs/message-broker.md). Phase 1 stores
// and validates it; nothing yet derives obligations from it.
type Envelope struct {
	Kind        string              `json:"kind"`
	To          string              `json:"to,omitempty"`
	Subject     string              `json:"subject"`
	Refs        map[string]string   `json:"refs,omitempty"`
	Body        EnvelopeBody        `json:"body"`
	Evidence    map[string]Evidence `json:"evidence,omitempty"`
	Attachments []string            `json:"attachments,omitempty"`
	Due         string              `json:"due,omitempty"`
}

// EnvelopeBody holds the named fields of every kind. Maps use short named keys
// (a1, e2) so each entry can be addressed directly, never by position.
type EnvelopeBody struct {
	Objective  string            `json:"objective,omitempty"`
	Owns       []string          `json:"owns,omitempty"`
	Ask        string            `json:"ask,omitempty"`
	Candidate  string            `json:"candidate,omitempty"`
	Scope      string            `json:"scope,omitempty"`
	Question   string            `json:"question,omitempty"`
	Options    map[string]string `json:"options,omitempty"`
	Outcome    string            `json:"outcome,omitempty"`
	Answer     string            `json:"answer,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	Needs      string            `json:"needs,omitempty"`
	ResumeWhen string            `json:"resumeWhen,omitempty"`
	Severity   string            `json:"severity,omitempty"`
	Summary    string            `json:"summary,omitempty"`
	Text       string            `json:"text,omitempty"`
	Acceptance map[string]string `json:"acceptance,omitempty"`
	Status     map[string]string `json:"status,omitempty"`
}

type Evidence struct {
	Type    string `json:"type"`
	Value   string `json:"value"`
	Outcome string `json:"outcome,omitempty"`
}

// Problem is one reason an envelope is invalid, addressed by field path.
type Problem struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

func (p Problem) String() string { return p.Field + ": " + p.Reason }

// EnvelopeError reports an invalid typed message. It matches ErrInvalid.
type EnvelopeError struct {
	Problems []Problem
}

func (e *EnvelopeError) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = p.String()
	}
	return "invalid envelope: " + strings.Join(parts, "; ")
}

func (e *EnvelopeError) Is(target error) bool { return target == ErrInvalid }

// NormalizeEnvelopePost validates a typed post and fills its display text.
// Text must be empty or exactly the rendered text, so the two never disagree.
func NormalizeEnvelopePost(req *PostMessageRequest) error {
	if req.Envelope == nil {
		return nil
	}
	if problems := ValidateEnvelope(*req.Envelope); problems != nil {
		return &EnvelopeError{Problems: problems}
	}
	rendered := RenderText(*req.Envelope)
	if req.Text != "" && req.Text != rendered {
		return &EnvelopeError{Problems: []Problem{{Field: "text", Reason: "must be empty or equal the envelope's rendered text"}}}
	}
	req.Text = rendered
	return nil
}

const (
	EnvelopeKindAssign   = "assign"
	EnvelopeKindRequest  = "request"
	EnvelopeKindReview   = "review"
	EnvelopeKindQuestion = "question"
	EnvelopeKindResult   = "result"
	EnvelopeKindAnswer   = "answer"
	EnvelopeKindBlock    = "block"
	EnvelopeKindDecline  = "decline"
	EnvelopeKindFinding  = "finding"
	EnvelopeKindNotice   = "notice"

	MaxEnvelopeBodyBytes = 4 << 10
	MaxEnvelopeBytes     = 8 << 10
	MaxEnvelopeDue       = 7 * 24 * time.Hour
)

// EnvelopeKinds lists every kind in documentation order.
var EnvelopeKinds = []string{
	EnvelopeKindAssign, EnvelopeKindRequest, EnvelopeKindReview, EnvelopeKindQuestion, EnvelopeKindResult,
	EnvelopeKindAnswer, EnvelopeKindBlock, EnvelopeKindDecline, EnvelopeKindFinding, EnvelopeKindNotice,
}

var (
	namedKey     = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)
	refKey       = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,31}$`)
	subjectID    = regexp.MustCompile(`\b[a-z]{2,5}_[0-9a-f]{6,}\b`)
	subjectHex   = regexp.MustCompile(`\b[0-9a-f]{7,}\b`)
	subjectDigit = regexp.MustCompile(`[0-9]`)
	subjectPath  = regexp.MustCompile(`(^|\s)(/|~/)[^\s]*/`)
)

var (
	evidenceTypes  = map[string]bool{"command": true, "commit": true, "file": true, "record": true, "url": true}
	statusValues   = map[string]bool{"pass": true, "fail": true, "partial": true}
	outcomeValues  = map[string]bool{"done": true, "partial": true}
	severityValues = map[string]bool{"low": true, "medium": true, "high": true, "critical": true}
)

// ValidateEnvelope is pure and deterministic. It returns every problem found,
// in a stable order; nil means the envelope is valid.
func ValidateEnvelope(e Envelope) []Problem {
	var out []Problem
	add := func(field, format string, args ...any) {
		out = append(out, Problem{Field: field, Reason: fmt.Sprintf(format, args...)})
	}
	required := func(field, value string) {
		if strings.TrimSpace(value) == "" {
			add(field, "required for %s", e.Kind)
		}
	}
	requiredMap := func(field string, m map[string]string) {
		if len(m) == 0 {
			add(field, "at least one entry is required for %s", e.Kind)
		}
	}

	kindKnown := false
	for _, k := range EnvelopeKinds {
		kindKnown = kindKnown || k == e.Kind
	}
	if !kindKnown {
		add("kind", "must be one of %s", strings.Join(EnvelopeKinds, ", "))
	}

	subject := strings.TrimSpace(e.Subject)
	switch n := utf8.RuneCountInString(subject); {
	case n < 10:
		add("subject", "must be at least 10 characters")
	case n > 120:
		add("subject", "must be at most 120 characters")
	}
	if strings.ContainsAny(e.Subject, "\r\n") {
		add("subject", "must be one line")
	}
	if subjectID.MatchString(subject) {
		add("subject", "must not contain record IDs; put them in refs")
	}
	for _, h := range subjectHex.FindAllString(subject, -1) {
		if subjectDigit.MatchString(h) {
			add("subject", "must not contain hashes; put them in refs")
			break
		}
	}
	if subjectPath.MatchString(subject) {
		add("subject", "must not contain paths; put them in refs")
	}

	for _, k := range sortedKeys(e.Refs) {
		if !refKey.MatchString(k) {
			add("refs."+k, "key must be a short name such as item, order or commit")
		}
		if strings.TrimSpace(e.Refs[k]) == "" {
			add("refs."+k, "must not be empty")
		}
	}
	checkNamed := func(field string, m map[string]string, allowed map[string]bool) {
		for _, k := range sortedKeys(m) {
			if !namedKey.MatchString(k) {
				add(field+"."+k, "key must match %s, for example a1", namedKey)
			}
			v := strings.TrimSpace(m[k])
			if v == "" {
				add(field+"."+k, "must not be empty")
			} else if allowed != nil && !allowed[v] {
				add(field+"."+k, "must be one of %s", joinKeys(allowed))
			}
		}
	}
	checkNamed("body.acceptance", e.Body.Acceptance, nil)
	checkNamed("body.status", e.Body.Status, statusValues)
	checkNamed("body.options", e.Body.Options, nil)
	for _, k := range sortedKeys(e.Evidence) {
		ev := e.Evidence[k]
		if !namedKey.MatchString(k) {
			add("evidence."+k, "key must match %s, for example e1", namedKey)
		}
		if !evidenceTypes[ev.Type] {
			add("evidence."+k+".type", "must be one of %s", joinKeys(evidenceTypes))
		}
		if strings.TrimSpace(ev.Value) == "" {
			add("evidence."+k+".value", "must not be empty")
		}
	}
	for i, a := range e.Attachments {
		if strings.TrimSpace(a) == "" {
			add(fmt.Sprintf("attachments.%d", i), "must not be empty")
		}
	}
	for i, o := range e.Body.Owns {
		if strings.TrimSpace(o) == "" {
			add(fmt.Sprintf("body.owns.%d", i), "must not be empty")
		}
	}
	if e.Due != "" {
		if d, err := time.ParseDuration(e.Due); err != nil || d <= 0 || d > MaxEnvelopeDue {
			add("due", "must be a positive duration up to 168h, such as 45m or 2h")
		}
	}

	b := e.Body
	switch e.Kind {
	case EnvelopeKindAssign:
		required("body.objective", b.Objective)
		if len(b.Owns) == 0 {
			add("body.owns", "at least one owned file or artifact is required for assign")
		}
		requiredMap("body.acceptance", b.Acceptance)
	case EnvelopeKindRequest:
		required("body.ask", b.Ask)
	case EnvelopeKindReview:
		required("body.candidate", b.Candidate)
		required("body.scope", b.Scope)
		requiredMap("body.acceptance", b.Acceptance)
	case EnvelopeKindQuestion:
		required("body.question", b.Question)
		if strings.Count(b.Question, "?") > 1 {
			add("body.question", "must ask exactly one question")
		}
	case EnvelopeKindResult:
		if !outcomeValues[b.Outcome] {
			add("body.outcome", "must be one of %s", joinKeys(outcomeValues))
		}
		requiredMap("body.status", b.Status)
		if len(e.Evidence) == 0 {
			add("evidence", "at least one entry is required for result")
		}
	case EnvelopeKindAnswer:
		required("body.answer", b.Answer)
	case EnvelopeKindBlock:
		required("body.reason", b.Reason)
		required("body.needs", b.Needs)
		required("body.resumeWhen", b.ResumeWhen)
	case EnvelopeKindDecline:
		required("body.reason", b.Reason)
	case EnvelopeKindFinding:
		if !severityValues[b.Severity] {
			add("body.severity", "must be one of %s", joinKeys(severityValues))
		}
		required("body.summary", b.Summary)
		if len(e.Evidence) == 0 {
			add("evidence", "at least one entry is required for finding")
		}
	case EnvelopeKindNotice:
		required("body.text", b.Text)
	}

	if body, err := json.Marshal(e.Body); err == nil && len(body) > MaxEnvelopeBodyBytes {
		add("body", "must be at most %d bytes; put longer material in a file and cite it in refs", MaxEnvelopeBodyBytes)
	}
	if whole, err := json.Marshal(e); err == nil && len(whole) > MaxEnvelopeBytes {
		add("envelope", "must be at most %d bytes", MaxEnvelopeBytes)
	}
	return out
}

// RenderText is the deterministic plain-text form stored as the message text,
// so readers that predate envelopes keep working. ParseTextConvention accepts it.
func RenderText(e Envelope) string {
	var b strings.Builder
	line := func(label, value string) {
		if value = oneLine(value); value != "" {
			b.WriteString("\n" + label + ": " + value)
		}
	}
	b.WriteString(strings.ToUpper(e.Kind) + ": " + oneLine(e.Subject))
	line("To", e.To)
	line("Refs", joinPairs(e.Refs, "="))
	body := e.Body
	line("Objective", body.Objective)
	line("Owns", strings.Join(body.Owns, ", "))
	line("Ask", body.Ask)
	line("Candidate", body.Candidate)
	line("Scope", body.Scope)
	line("Question", body.Question)
	line("Options", joinPairs(body.Options, ": "))
	line("Outcome", body.Outcome)
	line("Answer", body.Answer)
	line("Reason", body.Reason)
	line("Needs", body.Needs)
	line("Resume-when", body.ResumeWhen)
	line("Severity", body.Severity)
	line("Summary", body.Summary)
	line("Text", body.Text)
	line("Acceptance", joinPairs(body.Acceptance, ": "))
	line("Status", joinPairs(body.Status, " "))
	var ev []string
	for _, k := range sortedKeys(e.Evidence) {
		item := e.Evidence[k]
		s := k + " (" + item.Type + "): " + oneLine(item.Value)
		if item.Outcome != "" {
			s += " → " + oneLine(item.Outcome)
		}
		ev = append(ev, s)
	}
	line("Evidence", strings.Join(ev, "; "))
	line("Attachments", strings.Join(e.Attachments, ", "))
	line("Due", e.Due)
	return b.String()
}

var (
	conventionHead  = regexp.MustCompile(`^([A-Z]+):\s*(.*)$`)
	conventionField = regexp.MustCompile(`^([A-Za-z][A-Za-z-]*):\s*(.*)$`)
	namedPair       = regexp.MustCompile(`^([a-z][a-z0-9]{0,15})\s*(?:\(([a-z]+)\))?\s*[: ]\s*(.*)$`)
	hexToken        = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
)

// ParseTextConvention recognizes the phase-0 text convention ("KIND: subject"
// then "Field: value" lines). matched is false when the first line is not a
// known kind; otherwise the parsed envelope is returned for validation.
func ParseTextConvention(text string) (e Envelope, matched bool) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	head := conventionHead.FindStringSubmatch(strings.TrimSpace(lines[0]))
	if head == nil {
		return e, false
	}
	e.Kind = strings.ToLower(head[1])
	known := false
	for _, k := range EnvelopeKinds {
		known = known || k == e.Kind
	}
	if !known {
		return Envelope{}, false
	}
	e.Subject = strings.TrimSpace(head[2])
	var last *string
	for _, raw := range lines[1:] {
		l := strings.TrimSpace(raw)
		m := conventionField.FindStringSubmatch(l)
		if m == nil {
			if last != nil && l != "" {
				*last += " " + l // wrapped continuation of the previous field
			}
			continue
		}
		value := strings.TrimSpace(m[2])
		b := &e.Body
		last = nil
		switch strings.ToLower(m[1]) {
		case "to":
			e.To, last = value, &e.To
		case "refs":
			e.Refs = parseRefs(value)
		case "objective":
			b.Objective, last = value, &b.Objective
		case "owns":
			b.Owns = splitList(value)
		case "ask":
			b.Ask, last = value, &b.Ask
		case "candidate":
			b.Candidate, last = value, &b.Candidate
		case "scope":
			b.Scope, last = value, &b.Scope
		case "question":
			b.Question, last = value, &b.Question
		case "options":
			b.Options = parseNamed(splitSemicolons(value))
		case "outcome":
			b.Outcome = strings.ToLower(value)
		case "answer":
			b.Answer, last = value, &b.Answer
		case "reason":
			b.Reason, last = value, &b.Reason
		case "needs":
			b.Needs, last = value, &b.Needs
		case "resume-when":
			b.ResumeWhen, last = value, &b.ResumeWhen
		case "severity":
			b.Severity = strings.ToLower(value)
		case "summary":
			b.Summary, last = value, &b.Summary
		case "text":
			b.Text, last = value, &b.Text
		case "acceptance":
			b.Acceptance = parseNamed(splitSemicolons(value))
		case "status":
			b.Status = parseNamed(splitList(value))
			for k, v := range b.Status {
				b.Status[k] = strings.ToLower(v)
			}
		case "evidence":
			e.Evidence = parseEvidence(value)
		case "attachments":
			e.Attachments = splitList(value)
		case "due":
			e.Due = value
		}
	}
	return e, true
}

func parseRefs(value string) map[string]string {
	out := map[string]string{}
	for _, part := range splitList(value) {
		if k, v, ok := strings.Cut(part, "="); ok && refKey.MatchString(strings.TrimSpace(k)) {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		} else if k, v, ok := strings.Cut(part, " "); ok && refKey.MatchString(k) {
			out[k] = strings.TrimSpace(v)
		} else {
			out["ref"+strconv.Itoa(len(out)+1)] = part
		}
	}
	return out
}

func parseNamed(parts []string) map[string]string {
	out := map[string]string{}
	for _, part := range parts {
		if m := namedPair.FindStringSubmatch(part); m != nil {
			out[m[1]] = strings.TrimSpace(m[3])
		}
	}
	return out
}

// ParseEvidence reads "e1: value -> outcome; e2 (commit): abc1234" entries.
func ParseEvidence(value string) map[string]Evidence { return parseEvidence(value) }

func parseEvidence(value string) map[string]Evidence {
	out := map[string]Evidence{}
	for _, part := range splitSemicolons(value) {
		m := namedPair.FindStringSubmatch(part)
		if m == nil {
			continue
		}
		item := Evidence{Type: m[2], Value: strings.TrimSpace(m[3])}
		for _, arrow := range []string{"→", "->"} {
			if v, o, ok := strings.Cut(item.Value, arrow); ok {
				item.Value, item.Outcome = strings.TrimSpace(v), strings.TrimSpace(o)
				if item.Type == "" {
					item.Type = "command"
				}
				break
			}
		}
		if item.Type == "" {
			item.Type = inferEvidenceType(item.Value)
		}
		out[m[1]] = item
	}
	return out
}

func inferEvidenceType(v string) string {
	switch {
	case strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://"):
		return "url"
	case hexToken.MatchString(v):
		return "commit"
	case strings.HasPrefix(v, "/") || strings.HasPrefix(v, "~/") || strings.HasPrefix(v, "./"):
		return "file"
	default:
		return "record"
	}
}

// splitList splits on semicolons, or on commas when no semicolon is present.
func splitList(value string) []string {
	if strings.Contains(value, ";") {
		return splitSemicolons(value)
	}
	var out []string
	for _, p := range strings.Split(value, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func splitSemicolons(value string) []string {
	var out []string
	for _, p := range strings.Split(value, ";") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func joinPairs(m map[string]string, sep string) string {
	var parts []string
	for _, k := range sortedKeys(m) {
		parts = append(parts, k+sep+oneLine(m[k]))
	}
	return strings.Join(parts, "; ")
}

func joinKeys(m map[string]bool) string {
	return strings.Join(sortedKeys(m), ", ")
}

// sortedKeys orders keys naturally, so a2 sorts before a10.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return naturalLess(keys[i], keys[j]) })
	return keys
}

func naturalLess(a, b string) bool {
	pa, na := splitTrailingNumber(a)
	pb, nb := splitTrailingNumber(b)
	if pa != pb || na < 0 || nb < 0 {
		return a < b
	}
	return na < nb
}

func splitTrailingNumber(s string) (string, int) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	if i == len(s) {
		return s, -1
	}
	n, err := strconv.Atoi(s[i:])
	if err != nil {
		return s, -1
	}
	return s[:i], n
}
