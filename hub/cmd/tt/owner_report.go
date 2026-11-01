package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/scs32/tailterm/hub/internal/api"
	"github.com/scs32/tailterm/hub/internal/spawn"
)

// The owner report (wi_91bdb5b6f0f46f97, owner #51832) is rendered by the
// product from one fixed template, never written freehand, so it has the same
// shape whichever model or person runs it. docs/owner-report.md is the
// reference for every section, fixed line and window below.

const ownerReportUsage = "usage: tt owner report hourly|morning|evening [--task ID] [--tz ZONE] [--host H] [--now T] [--since T] [--post]\n" +
	"  --task is the project ID; required unless TAILTERM_TASK is set\n" +
	"  --tz is the IANA zone for the window and times (default: this machine's zone)\n" +
	"  --host names the provider-usage host (default: this machine)\n" +
	"  --now and --since are RFC3339 times: the window's end and an optional start override\n" +
	"  --post ends the window at the kind's slot at or before --now and posts the report to the Board once per slot"

// Fixed lines. Each is printed verbatim; docs/owner-report.md quotes them.
const (
	ownerReportNotYet         = "- not yet available (waits for wi_475a)"
	ownerReportNoShipped      = "- none in this window"
	ownerReportNothingQueued  = "- nothing queued"
	ownerReportNoRunning      = "- none running"
	ownerReportNothingWrong   = "- none recorded (sources: refused or rolled-back releases, owner interventions)"
	ownerReportNothingWaiting = "- nothing waiting on you"
	ownerReportNoCompletion   = "no saved completion report"
	ownerReportNoneRecorded   = "none recorded"
	ownerReportTextLimit      = 280
	// The Shipped line is a summary: the first sentence of each field,
	// cut at these rune limits (wi_546279658420371a f1c).
	ownerReportDeliveredLimit  = 160
	ownerReportLimitsLimit     = 120
	ownerReportCallTimeout     = 10 * time.Second
	ownerReportSettledPageSize = 200
)

// Source names used in "- unavailable: could not read <source>" lines.
const (
	ownerSourceReleases      = "the release ledger"
	ownerSourceTeamQueue     = "the team queue"
	ownerSourceInterventions = "owner interventions"
	ownerSourceProvider      = "provider usage"
	ownerSourceProjectUsage  = "project usage"
	ownerSourceObligations   = "owner obligations"
	ownerSourceNarrative     = "the work item narrative"
)

var ownerReportKinds = map[string]string{"hourly": "Hourly", "morning": "Morning", "evening": "Evening"}

type ownerReportShipped struct {
	at                     time.Time
	item, title            string
	delivered, limitations string
	noReport, unavailable  bool
}

type ownerReportRunning struct {
	position                     int64
	item, title, template, state string
}

type ownerReportUpNext struct {
	n                     int
	item, title, template string
}

type ownerReportWrong struct {
	at   time.Time
	key  string
	line string
}

// ownerReportData is everything the renderer prints, already fetched,
// filtered to the window and sorted. Unavailable lists name sources whose
// read failed; the renderer prints a fixed line for each.
type ownerReportData struct {
	kind       string
	start, end time.Time
	zone       *time.Location

	shipped            []ownerReportShipped
	shippedUnavailable bool

	released, rolledBack, refused, superseded, inLine int
	releasesUnavailable                               bool

	running            []ownerReportRunning
	runningLimit       int
	runningUnavailable bool // the team queue read failed: Running teams and Up next

	upNext []ownerReportUpNext

	wrong            []ownerReportWrong
	wrongUnavailable []string

	host                string
	readings            []api.ProviderReading
	providerUnavailable bool
	tokens              map[string]string
	tokensUnavailable   bool

	needs            []api.Obligation
	needsUnavailable bool
}

// renderOwnerReport prints the fixed template. It reads no clock, no
// environment and no unsorted map, so equal data gives equal bytes.
func renderOwnerReport(d ownerReportData) string {
	var b strings.Builder
	line := func(format string, args ...any) {
		fmt.Fprintf(&b, format, args...)
		b.WriteByte('\n')
	}
	unavailable := func(source string) { line("- unavailable: could not read %s", source) }
	count := func(n int, bad bool) string {
		if bad {
			return ""
		}
		return fmt.Sprintf(" (%d)", n)
	}
	start, end := d.start.In(d.zone), d.end.In(d.zone)
	span := start.Format("2006-01-02 15:04") + "–"
	if start.Format("2006-01-02") != end.Format("2006-01-02") {
		span += end.Format("2006-01-02 ")
	}
	span += end.Format("15:04") + " " + end.Format("MST")
	line("**Owner report: %s** %s", ownerReportKinds[d.kind], span)

	line("**Shipped**%s", count(len(d.shipped), d.shippedUnavailable))
	switch {
	case d.shippedUnavailable:
		unavailable(ownerSourceReleases)
	case len(d.shipped) == 0:
		line(ownerReportNoShipped)
	}
	for _, s := range d.shipped {
		head := s.item
		if s.title != "" {
			head += " " + s.title
		}
		switch {
		case s.unavailable:
			line("- %s: unavailable: could not read %s", head, ownerSourceNarrative)
		case s.noReport:
			line("- %s: %s", head, ownerReportNoCompletion)
		default:
			delivered := strings.TrimRight(orNone(reportSummary(s.delivered, ownerReportDeliveredLimit)), ".")
			line("- %s: %s. Limits: %s", head, delivered, orNone(reportSummary(s.limitations, ownerReportLimitsLimit)))
		}
	}

	line("**Releases**")
	if d.releasesUnavailable {
		unavailable(ownerSourceReleases)
	} else {
		line("- released %d, rolled back %d, refused %d, superseded %d in this window; in line now: %d", d.released, d.rolledBack, d.refused, d.superseded, d.inLine)
	}

	if d.runningUnavailable {
		line("**Running teams**")
		unavailable(ownerSourceTeamQueue)
	} else {
		line("**Running teams** (%d of %d)", len(d.running), d.runningLimit)
		if len(d.running) == 0 {
			line(ownerReportNoRunning)
		}
	}
	for _, r := range d.running {
		head := r.item
		if r.title != "" {
			head += " " + r.title
		}
		line("- %s (%s, %s)", head, r.template, r.state)
	}

	if d.runningUnavailable {
		line("**Up next**")
		unavailable(ownerSourceTeamQueue)
	} else {
		line("**Up next** (%d)", len(d.upNext))
		if len(d.upNext) == 0 {
			line(ownerReportNothingQueued)
		}
	}
	for _, u := range d.upNext {
		head := u.item
		if u.title != "" {
			head += " " + u.title
		}
		line("- %d. %s (%s)", u.n, head, u.template)
	}

	line("**What went wrong**%s", count(len(d.wrong), len(d.wrongUnavailable) > 0))
	for _, source := range d.wrongUnavailable {
		unavailable(source)
	}
	if len(d.wrong) == 0 && len(d.wrongUnavailable) == 0 {
		line(ownerReportNothingWrong)
	}
	for _, w := range d.wrong {
		line("%s", w.line)
	}

	line("**Opened vs closed**")
	line(ownerReportNotYet)

	line("**Usage**")
	switch {
	case d.providerUnavailable:
		unavailable(ownerSourceProvider)
	case len(d.readings) == 0:
		line("- no provider reading for host %s", d.host)
	}
	for _, r := range d.readings {
		if r.State != api.ProviderUsageOK || r.UsedPercent == "" {
			line("- %s %s: %s", r.Runtime, r.Window, orNone(r.State))
			continue
		}
		text := fmt.Sprintf("- %s %s: %s%% used", r.Runtime, r.Window, r.UsedPercent)
		if t, ok := parseReportTime(r.ResetsAt); ok {
			text += ", resets " + t.In(d.zone).Format("Mon 15:04")
		}
		captured := r.CapturedAt
		if captured == "" {
			captured = r.ReportedAt
		}
		if t, ok := parseReportTime(captured); ok {
			text += " (reading " + reportAge(d.end.Sub(t)) + " old)"
		}
		line("%s", text)
	}
	if d.tokensUnavailable {
		unavailable(ownerSourceProjectUsage)
	} else {
		classes := make([]string, 0, len(d.tokens))
		for class := range d.tokens {
			classes = append(classes, class)
		}
		sort.Strings(classes)
		parts := make([]string, 0, len(classes))
		for _, class := range classes {
			parts = append(parts, class+" "+d.tokens[class])
		}
		if len(parts) == 0 {
			parts = []string{ownerReportNoneRecorded}
		}
		line("- project tokens in window: %s", strings.Join(parts, ", "))
	}

	line("**Host health**")
	line(ownerReportNotYet)

	line("**Needs you**%s", count(len(d.needs), d.needsUnavailable))
	switch {
	case d.needsUnavailable:
		unavailable(ownerSourceObligations)
	case len(d.needs) == 0:
		line(ownerReportNothingWaiting)
	}
	for _, o := range d.needs {
		what := o.Subject
		if strings.TrimSpace(what) == "" {
			what = o.Needs
		}
		due := o.DueAt.In(d.zone)
		dueText := due.Format("15:04")
		if due.Format("2006-01-02") != end.Format("2006-01-02") {
			dueText = due.Format("Jan 2 15:04")
		}
		if o.Overdue != "" {
			dueText += ", overdue " + o.Overdue
		}
		line("- #%d %s: %s (due %s)", o.MessageSeq, o.SourceKind, reportText(what), dueText)
	}
	return b.String()
}

func orNone(s string) string {
	if s = reportText(s); s == "" {
		return ownerReportNoneRecorded
	}
	return s
}

// reportText collapses whitespace and cuts saved free text at a fixed length.
func reportText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= ownerReportTextLimit {
		return s
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:ownerReportTextLimit]), " ") + "…"
}

// reportSummary is the first sentence of saved free text, whitespace
// collapsed, cut at limit runes and then ending in "…". A sentence ends at
// ".", "!" or "?" followed by a space or the end of the text.
func reportSummary(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	for i := 0; i < len(s); i++ {
		if (s[i] == '.' || s[i] == '!' || s[i] == '?') && (i+1 == len(s) || s[i+1] == ' ') {
			s = s[:i+1]
			break
		}
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:limit]), " ") + "…"
}

func parseReportTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}

// reportAge is a whole-minute duration such as 7m or 3h05m.
func reportAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	m := int64(d / time.Minute)
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dh%02dm", m/60, m%60)
}

// ownerReportWindow returns [start, end) for a report kind: hourly is the
// hour before now, morning starts at the latest 21:00 strictly before now,
// evening at the latest 07:00 at or before now.
func ownerReportWindow(kind string, now time.Time, zone *time.Location) time.Time {
	local := now.In(zone)
	at := func(hour int) time.Time {
		return time.Date(local.Year(), local.Month(), local.Day(), hour, 0, 0, 0, zone)
	}
	switch kind {
	case "morning":
		start := at(21)
		if !start.Before(now) {
			start = start.AddDate(0, 0, -1)
		}
		return start
	case "evening":
		start := at(7)
		if start.After(now) {
			start = start.AddDate(0, 0, -1)
		}
		return start
	}
	return now.Add(-time.Hour)
}

// ownerReportSlot is the scheduled end of the kind's latest slot at or
// before now: the top of the local hour for hourly, the latest 07:00 local
// for morning and the latest 21:00 local for evening. --post reports that
// slot's window, so a wake read late still reports exactly its slot.
func ownerReportSlot(kind string, now time.Time, zone *time.Location) time.Time {
	local := now.In(zone)
	at := func(hour int) time.Time {
		t := time.Date(local.Year(), local.Month(), local.Day(), hour, 0, 0, 0, zone)
		if t.After(now) {
			t = t.AddDate(0, 0, -1)
		}
		return t
	}
	switch kind {
	case "morning":
		return at(7)
	case "evening":
		return at(21)
	}
	into := time.Duration(local.Minute())*time.Minute + time.Duration(local.Second())*time.Second + time.Duration(local.Nanosecond())
	return now.Add(-into)
}

// ownerReportRequestID is the post's retry identity: one per project, kind
// and window, so posting the same slot again finds the first post.
func ownerReportRequestID(task, kind string, start, end time.Time) string {
	sum := sha256.Sum256([]byte(task + "\x00" + kind + "\x00" + start.UTC().Format(time.RFC3339Nano) + "\x00" + end.UTC().Format(time.RFC3339Nano)))
	return "owner-report-" + kind + "-" + hex.EncodeToString(sum[:12])
}

func inReportWindow(t, start, end time.Time) bool {
	return !t.Before(start) && t.Before(end)
}

// cmdOwnerReport renders the owner's progress report. Without --post it only
// reads, so the owner terminal and agent sessions (the owner helper) may both
// run it. With --post it also posts the printed bytes to the Board as the
// caller, once per slot: this is the fixed step a hub wake's Board message
// names (docs/owner-report.md, "Sending on the hub schedule").
func cmdOwnerReport(e env, args []string) error {
	if len(args) == 0 || ownerReportKinds[args[0]] == "" {
		return errors.New(ownerReportUsage)
	}
	kind := args[0]
	fs := flag.NewFlagSet("owner report", flag.ContinueOnError)
	task := fs.String("task", e.task, "project id")
	zoneName := fs.String("tz", "", "IANA time zone")
	host := fs.String("host", "", "provider-usage host")
	nowFlag := fs.String("now", "", "window end, RFC3339")
	sinceFlag := fs.String("since", "", "window start override, RFC3339")
	post := fs.Bool("post", false, "end the window at the kind's slot and post the report to the Board once per slot")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 || *task == "" {
		return errors.New(ownerReportUsage)
	}
	if *zoneName == "" {
		*zoneName = localTimeZoneName()
	}
	zone, err := time.LoadLocation(*zoneName)
	if err != nil {
		return fmt.Errorf("--tz %q: %w", *zoneName, err)
	}
	now := time.Now()
	if *nowFlag != "" {
		if now, err = time.Parse(time.RFC3339Nano, *nowFlag); err != nil {
			return fmt.Errorf("--now must be RFC3339: %w", err)
		}
	}
	if *post {
		now = ownerReportSlot(kind, now, zone)
	}
	start := ownerReportWindow(kind, now, zone)
	if *sinceFlag != "" {
		if start, err = time.Parse(time.RFC3339Nano, *sinceFlag); err != nil {
			return fmt.Errorf("--since must be RFC3339: %w", err)
		}
		if !start.Before(now) {
			return errors.New("--since must be before --now")
		}
	}
	if *host == "" {
		*host = spawn.Host()
	}
	c, err := e.client(ownerReportCallTimeout)
	if err != nil {
		return err
	}
	d, failures := fetchOwnerReport(c, *task, kind, start, now, zone, *host)
	report := renderOwnerReport(d)
	fmt.Print(report)
	if *post {
		// A report with unavailable sections is still posted: the owner sees
		// what could not be read, and the command still exits 1 below.
		seq, already, err := postOwnerReport(c, e, *task, ownerReportRequestID(*task, kind, start, now), report)
		if err != nil {
			return fmt.Errorf("the report was not posted: %w", err)
		}
		if already {
			fmt.Printf("already posted #%d for this slot\n", seq)
		} else {
			fmt.Printf("posted #%d\n", seq)
		}
	}
	if len(failures) > 0 {
		for _, f := range failures {
			fmt.Fprintf(os.Stderr, "tt owner report: %s\n", f)
		}
		return fmt.Errorf("%d report source(s) could not be read; the report shows each as unavailable", len(failures))
	}
	return nil
}

// postOwnerReport posts the report as one plain undirected Board message
// under requestID. A slot that was already posted (even with other bytes,
// because the data has moved since) is found by its receipt and not posted
// again; already reports that case.
func postOwnerReport(c *api.Client, e env, task, requestID, report string) (seq int64, already bool, err error) {
	found := func() (int64, bool) {
		ctx, cancel := ctxTimeout(ownerReportCallTimeout)
		defer cancel()
		m, err := c.GetMessagePostReceipt(ctx, task, requestID, e.agent)
		if err != nil || m.PostReceipt == nil || m.PostReceipt.RequestID != requestID || m.PostReceipt.MessageSeq <= 0 {
			return 0, false
		}
		return m.PostReceipt.MessageSeq, true
	}
	if seq, ok := found(); ok {
		return seq, true, nil
	}
	ctx, cancel := ctxTimeout(ownerReportCallTimeout)
	defer cancel()
	m, err := c.PostMessage(ctx, task, api.PostMessageRequest{Text: report, AgentID: e.agent, RunID: e.runID, RequestID: requestID})
	if err != nil {
		// A concurrent run may have posted the slot first.
		if seq, ok := found(); ok {
			return seq, true, nil
		}
		return 0, false, err
	}
	recordUsageContext(e, task, "post", m.Seq)
	return m.Seq, false, nil
}

// fetchOwnerReport reads each source independently; a failed read marks only
// its own section unavailable and is returned as a failure.
func fetchOwnerReport(c *api.Client, task, kind string, start, end time.Time, zone *time.Location, host string) (ownerReportData, []string) {
	d := ownerReportData{kind: kind, start: start, end: end, zone: zone, host: host}
	var failures []string
	fail := func(source string, err error) {
		failures = append(failures, fmt.Sprintf("could not read %s: %v", source, err))
	}
	call := func(fn func(context.Context) error) error {
		ctx, cancel := ctxTimeout(ownerReportCallTimeout)
		defer cancel()
		return fn(ctx)
	}

	// Narrative overviews are read once per item across Shipped, Running and
	// Up next.
	type overview struct {
		o   api.NarrativeOverview
		err error
	}
	overviews := map[string]overview{}
	readOverview := func(item string) overview {
		if o, ok := overviews[item]; ok {
			return o
		}
		var o overview
		o.err = call(func(ctx context.Context) (err error) {
			o.o, err = c.GetNarrativeOverview(ctx, task, item)
			return err
		})
		if o.err != nil {
			fail(ownerSourceNarrative+" of "+item, o.err)
		}
		overviews[item] = o
		return o
	}

	// Settled releases: every page, filtered by SettledAt. Jobs without a
	// settled time cannot be placed in a window and are left out.
	var settled []api.ReleaseSummary
	settledErr := func() error {
		opts := api.ReleaseListOptions{View: "settled", Limit: ownerReportSettledPageSize}
		for {
			var page api.ReleasePage
			if err := call(func(ctx context.Context) (err error) {
				page, err = c.ReleasesPage(ctx, task, opts)
				return err
			}); err != nil {
				return err
			}
			for _, j := range page.Jobs {
				if t, ok := parseReportTime(j.SettledAt); ok && inReportWindow(t, start, end) {
					settled = append(settled, j)
				}
			}
			if page.Page.NextAfter == "" {
				return nil
			}
			opts.After, opts.Snapshot = page.Page.NextAfter, page.Page.Snapshot
		}
	}()
	inLine := 0
	activeErr := func() error {
		opts := api.ReleaseListOptions{View: "active"}
		for {
			var page api.ReleasePage
			if err := call(func(ctx context.Context) (err error) {
				page, err = c.ReleasesPage(ctx, task, opts)
				return err
			}); err != nil {
				return err
			}
			inLine += len(page.Jobs)
			if page.Page.NextAfter == "" {
				return nil
			}
			opts.After, opts.Snapshot = page.Page.NextAfter, page.Page.Snapshot
		}
	}()
	if settledErr != nil {
		fail(ownerSourceReleases, settledErr)
		d.shippedUnavailable, d.releasesUnavailable = true, true
		d.wrongUnavailable = append(d.wrongUnavailable, ownerSourceReleases)
	} else {
		if activeErr != nil {
			fail(ownerSourceReleases, activeErr)
			d.releasesUnavailable = true
		}
		d.inLine = inLine
		sort.SliceStable(settled, func(i, j int) bool {
			ti, _ := parseReportTime(settled[i].SettledAt)
			tj, _ := parseReportTime(settled[j].SettledAt)
			if !ti.Equal(tj) {
				return ti.Before(tj)
			}
			return settled[i].ID < settled[j].ID
		})
		seen := map[string]bool{}
		for _, j := range settled {
			at, _ := parseReportTime(j.SettledAt)
			switch j.State {
			case "released":
				d.released++
			case "superseded":
				d.superseded++
			case "rolled_back":
				d.rolledBack++
				what := "rolled back"
				reason := ""
				if j.Receipt != nil && j.Receipt.Failure != nil {
					if j.Receipt.Failure.Step != "" {
						what += " at " + j.Receipt.Failure.Step
					}
					reason = j.Receipt.Failure.Reason
				}
				d.wrong = append(d.wrong, ownerReportWrong{at: at, key: j.ID, line: fmt.Sprintf("- release %s for %s %s: %s", j.ID, j.ItemID, what, orNone(reason))})
			case "refused":
				d.refused++
				what := "refused"
				reason := ""
				if j.Refusal != nil {
					if j.Refusal.Step != "" {
						what += " at " + j.Refusal.Step
					}
					reason = j.Refusal.Reason
				}
				d.wrong = append(d.wrong, ownerReportWrong{at: at, key: j.ID, line: fmt.Sprintf("- release %s for %s %s: %s", j.ID, j.ItemID, what, orNone(reason))})
			}
			// Shipped: an item whose change went live (released, or
			// released by hand and superseded), once, at its first time.
			if (j.State == "released" || j.State == "superseded") && !seen[j.ItemID] {
				seen[j.ItemID] = true
				d.shipped = append(d.shipped, ownerReportShipped{at: at, item: j.ItemID})
			}
		}
		for i := range d.shipped {
			s := &d.shipped[i]
			o := readOverview(s.item)
			if o.err != nil {
				s.unavailable = true
				continue
			}
			s.title = reportText(o.o.Item.Title)
			pin := o.o.CompletionReport
			if pin == nil {
				s.noReport = true
				continue
			}
			var report api.NarrativeReportVersion
			if err := call(func(ctx context.Context) (err error) {
				report, err = c.GetNarrativeReportVersion(ctx, task, s.item, pin.ReportID, pin.Version)
				return err
			}); err != nil {
				fail(ownerSourceNarrative+" of "+s.item, err)
				s.unavailable = true
				continue
			}
			s.delivered, s.limitations = report.Sections.DeliveredWork, report.Sections.Limitations
		}
	}

	var queue api.TeamQueueList
	if err := call(func(ctx context.Context) (err error) {
		queue, err = c.ListTeamQueuePage(ctx, task, api.TeamQueueListOptions{View: api.TeamQueueViewActive})
		return err
	}); err != nil {
		fail(ownerSourceTeamQueue, err)
		d.runningUnavailable = true
	} else {
		d.runningLimit = queue.ConcurrencyLimit
		for _, q := range queue.Entries {
			if q.State == "queued" && q.UpNext > 0 {
				u := ownerReportUpNext{n: q.UpNext, item: q.ItemID, template: q.Template}
				if o := readOverview(q.ItemID); o.err == nil {
					u.title = reportText(o.o.Item.Title)
				}
				d.upNext = append(d.upNext, u)
				continue
			}
			if q.State != "launching" && q.State != "running" {
				continue
			}
			r := ownerReportRunning{position: q.Position, item: q.ItemID, template: q.Template, state: q.State}
			if o := readOverview(q.ItemID); o.err == nil {
				r.title = reportText(o.o.Item.Title)
			}
			d.running = append(d.running, r)
		}
		sort.SliceStable(d.running, func(i, j int) bool {
			if d.running[i].position != d.running[j].position {
				return d.running[i].position < d.running[j].position
			}
			return d.running[i].item < d.running[j].item
		})
		sort.SliceStable(d.upNext, func(i, j int) bool {
			if d.upNext[i].n != d.upNext[j].n {
				return d.upNext[i].n < d.upNext[j].n
			}
			return d.upNext[i].item < d.upNext[j].item
		})
	}

	if err := func() error {
		for after := int64(0); ; {
			var page api.InterventionList
			if err := call(func(ctx context.Context) (err error) {
				page, err = c.ListInterventions(ctx, task, after, api.MaxInterventionPage, "")
				return err
			}); err != nil {
				return err
			}
			for _, m := range page.Interventions {
				if m.Intervention == nil || !inReportWindow(m.CreatedAt, start, end) {
					continue
				}
				iv := m.Intervention
				actor := iv.Actor
				if actor == "" {
					actor = "unknown"
				}
				d.wrong = append(d.wrong, ownerReportWrong{at: m.CreatedAt, key: fmt.Sprintf("#%020d", m.Seq), line: fmt.Sprintf("- intervention #%d %s on %s by %s", m.Seq, iv.Kind, iv.ItemID, actor)})
			}
			if page.NextAfter == 0 || page.NextAfter <= after {
				return nil
			}
			after = page.NextAfter
		}
	}(); err != nil {
		fail(ownerSourceInterventions, err)
		d.wrongUnavailable = append(d.wrongUnavailable, ownerSourceInterventions)
	}
	sort.SliceStable(d.wrong, func(i, j int) bool {
		if !d.wrong[i].at.Equal(d.wrong[j].at) {
			return d.wrong[i].at.Before(d.wrong[j].at)
		}
		return d.wrong[i].key < d.wrong[j].key
	})

	var usage api.ProviderUsage
	if err := call(func(ctx context.Context) (err error) {
		usage, err = c.ProviderUsage(ctx, host)
		return err
	}); err != nil {
		fail(ownerSourceProvider, err)
		d.providerUnavailable = true
	} else {
		d.readings = append([]api.ProviderReading(nil), usage.Readings...)
		sort.SliceStable(d.readings, func(i, j int) bool {
			if d.readings[i].Runtime != d.readings[j].Runtime {
				return d.readings[i].Runtime < d.readings[j].Runtime
			}
			return d.readings[i].Window < d.readings[j].Window
		})
	}
	var project api.UsageReport
	if err := call(func(ctx context.Context) (err error) {
		project, err = c.Usage(ctx, task, api.UsageQuery{From: start, To: end})
		return err
	}); err != nil {
		fail(ownerSourceProjectUsage, err)
		d.tokensUnavailable = true
	} else {
		d.tokens = project.Summary.Tokens
	}

	var needs []api.Obligation
	if err := call(func(ctx context.Context) (err error) {
		needs, err = c.ListOwnerObligations(ctx, task, true)
		return err
	}); err != nil {
		fail(ownerSourceObligations, err)
		d.needsUnavailable = true
	} else {
		d.needs = append([]api.Obligation(nil), needs...)
		sort.SliceStable(d.needs, func(i, j int) bool {
			if !d.needs[i].DueAt.Equal(d.needs[j].DueAt) {
				return d.needs[i].DueAt.Before(d.needs[j].DueAt)
			}
			return d.needs[i].MessageSeq < d.needs[j].MessageSeq
		})
	}
	return d, failures
}
