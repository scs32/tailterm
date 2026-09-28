package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/scs32/tailterm/hub/internal/api"
)

var usageDecimal = regexp.MustCompile(`^(0|[1-9][0-9]{0,12})(\.[0-9]{1,9})?$`)
var usageCurrency = regexp.MustCompile(`^[A-Z]{3}$`)

func usageRat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return new(big.Rat)
	}
	return r
}
func (s *Store) UsagePrices(ctx context.Context, task string) (api.UsagePrices, error) {
	return readUsagePrices(ctx, s.db, task)
}
func readUsagePrices(ctx context.Context, q queryRower, task string) (api.UsagePrices, error) {
	out := api.UsagePrices{Rows: []api.UsagePrice{}}
	if !api.ValidID(task, "tsk") {
		return out, api.ErrInvalid
	}
	var raw string
	err := q.QueryRowContext(ctx, `SELECT revision,payload FROM usage_price_revisions WHERE task_id=? ORDER BY revision DESC LIMIT 1`, task).Scan(&out.Revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	err = json.Unmarshal([]byte(raw), &out.Rows)
	return out, err
}
func (s *Store) SetUsagePrices(ctx context.Context, task string, req api.UsagePriceRequest) (api.UsagePrices, error) {
	zero := api.UsagePrices{}
	if !api.ValidID(task, "tsk") || !validRequestID(req.RequestID) || req.ExpectedRevision < 0 || len(req.Rows) > 200 {
		return zero, api.ErrInvalid
	}
	keys := map[string]bool{}
	for _, p := range req.Rows {
		if (p.Runtime != "codex" && p.Runtime != "claude") || p.Model == "" || len(p.Model) > 160 || !usageCurrency.MatchString(p.Currency) || p.EffectiveAt.IsZero() || len(p.Rates) == 0 {
			return zero, api.ErrInvalid
		}
		key := p.Runtime + "/" + p.Model + "/" + ts(p.EffectiveAt)
		if keys[key] {
			return zero, api.ErrInvalid
		}
		keys[key] = true
		for k, r := range p.Rates {
			if !usageClass(k) || !usageDecimal.MatchString(r) {
				return zero, api.ErrInvalid
			}
		}
	}
	raw, _ := json.Marshal(req)
	if len(raw) > 60<<10 {
		return zero, api.ErrInvalid
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	var old, result string
	err = tx.QueryRowContext(ctx, `SELECT payload,result FROM usage_price_receipts WHERE task_id=? AND request_id=?`, task, req.RequestID).Scan(&old, &result)
	if err == nil {
		if old != string(raw) {
			return zero, api.ErrConflict
		}
		err = json.Unmarshal([]byte(result), &zero)
		return zero, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}
	current, err := readUsagePrices(ctx, tx, task)
	if err != nil {
		return zero, err
	}
	if current.Revision != req.ExpectedRevision {
		return zero, api.ErrConflict
	}
	var exists int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE id=?`, task).Scan(&exists); err != nil {
		return zero, err
	}
	if exists != 1 {
		return zero, api.ErrNotFound
	}
	zero = api.UsagePrices{Revision: current.Revision + 1, Rows: req.Rows}
	rows, _ := json.Marshal(req.Rows)
	resultRaw, _ := json.Marshal(zero)
	if _, err = tx.ExecContext(ctx, `INSERT INTO usage_price_revisions VALUES(?,?,?)`, task, zero.Revision, string(rows)); err != nil {
		return api.UsagePrices{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO usage_price_receipts VALUES(?,?,?,?)`, task, req.RequestID, string(raw), string(resultRaw)); err != nil {
		return api.UsagePrices{}, err
	}
	err = tx.Commit()
	return zero, err
}

type usageAccumulator struct {
	summary         api.UsageSummary
	counts          map[string]bool
	allocated       *big.Rat
	tokens          map[string]*big.Rat
	costs           map[string]*big.Rat
	context         *big.Rat
	contextTurns    *big.Rat
	contextMeasured bool
	full            bool
}

func newUsageAccumulator() *usageAccumulator {
	return &usageAccumulator{summary: api.UsageSummary{State: "not measured", Tokens: map[string]string{}, MeasuredRequests: map[string]int{}, PricedSubtotal: map[string]string{}}, counts: map[string]bool{}, allocated: new(big.Rat), tokens: map[string]*big.Rat{}, costs: map[string]*big.Rat{}, context: new(big.Rat), contextTurns: new(big.Rat), contextMeasured: true, full: true}
}
func usagePriceFor(t api.UsageTurn, prices api.UsagePrices) *api.UsagePrice {
	var p *api.UsagePrice
	for i := range prices.Rows {
		x := &prices.Rows[i]
		if x.Runtime == t.Runtime && x.Model == t.Model && !x.EffectiveAt.After(t.At) && (p == nil || x.EffectiveAt.After(p.EffectiveAt)) {
			p = x
		}
	}
	return p
}
func (a *usageAccumulator) add(key string, t api.UsageTurn, d int64, prices api.UsagePrices) {
	if d < 1 {
		return
	}
	share := big.NewRat(1, d)
	fresh := !a.counts[key]
	a.counts[key] = true
	a.allocated.Add(a.allocated, share)
	if a.summary.State == "not measured" {
		a.summary.State = "measured"
	}
	if !t.Complete || t.Gap != "" || len(t.Tokens) < len(api.UsageClasses) {
		a.summary.State = "partial"
		a.full = false
	}
	price := usagePriceFor(t, prices)
	if price == nil {
		a.full = false
	}
	for _, class := range api.UsageClasses {
		v, ok := t.Tokens[class]
		if !ok {
			a.full = false
			continue
		}
		if fresh {
			a.summary.MeasuredRequests[class]++
		}
		amount := big.NewRat(v, d)
		if a.tokens[class] == nil {
			a.tokens[class] = new(big.Rat)
		}
		a.tokens[class].Add(a.tokens[class], amount)
		if price != nil {
			rate, priced := price.Rates[class]
			if !priced {
				a.full = false
				continue
			}
			cost := new(big.Rat).Mul(amount, usageRat(rate))
			cost.Quo(cost, big.NewRat(1000000, 1))
			if a.costs[price.Currency] == nil {
				a.costs[price.Currency] = new(big.Rat)
			}
			a.costs[price.Currency].Add(a.costs[price.Currency], cost)
		}
	}
	input, iok := t.Tokens["input"]
	cached, cok := t.Tokens["cached"]
	if iok && cok {
		n := new(big.Rat).Add(big.NewRat(input, d), big.NewRat(cached, d))
		a.context.Add(a.context, n)
		a.contextTurns.Add(a.contextTurns, share)
	} else {
		a.contextMeasured = false
	}
}
func (a *usageAccumulator) finish() api.UsageSummary {
	out := a.summary
	out.Requests = len(a.counts)
	out.AllocatedTurns = a.allocated.RatString()
	out.CostComplete = a.full && out.Requests > 0
	for k, v := range a.tokens {
		out.Tokens[k] = v.RatString()
	}
	for k, v := range a.costs {
		out.PricedSubtotal[k] = v.RatString()
	}
	if a.contextMeasured && a.contextTurns.Sign() > 0 {
		v := new(big.Rat).Quo(a.context, a.contextTurns).RatString()
		out.AverageContext = &v
		if a.context.Sign() > 0 {
			cached := a.tokens["cached"]
			if cached != nil {
				s := new(big.Rat).Quo(cached, a.context).RatString()
				out.CachedShare = &s
			}
		}
	}
	return out
}

type usageItemAccumulator struct {
	out                               api.UsageItemReport
	all                               *usageAccumulator
	phases, roles, models, phaseRoles map[string]*usageAccumulator
}

func newUsageItem(task, item, title string) *usageItemAccumulator {
	return &usageItemAccumulator{out: api.UsageItemReport{TaskID: task, ItemID: item, Title: title}, all: newUsageAccumulator(), phases: map[string]*usageAccumulator{}, roles: map[string]*usageAccumulator{}, models: map[string]*usageAccumulator{}, phaseRoles: map[string]*usageAccumulator{}}
}
func (a *usageItemAccumulator) add(key string, p api.UsageProjection, d int64, prices api.UsagePrices) {
	a.all.add(key, p.Turn, d, prices)
	phase := p.Phase
	if phase == "review" {
		if p.ReviewRound > 0 {
			phase += " round " + strconv.Itoa(p.ReviewRound)
		} else {
			phase += " round unavailable"
		}
	}
	model := p.Turn.Runtime + "/" + p.Turn.Model
	if p.Turn.Model == "" {
		model = p.Turn.Runtime + "/unknown"
	}
	for _, entry := range []struct {
		m map[string]*usageAccumulator
		k string
	}{{a.phases, phase}, {a.roles, p.Role}, {a.models, model}, {a.phaseRoles, phase + " / " + p.Role}} {
		if entry.m[entry.k] == nil {
			entry.m[entry.k] = newUsageAccumulator()
		}
		entry.m[entry.k].add(key, p.Turn, d, prices)
	}
}
func usageGroups(m map[string]*usageAccumulator) []api.UsageGroup {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []api.UsageGroup{}
	for _, k := range keys {
		out = append(out, api.UsageGroup{Key: k, Label: k, Summary: m[k].finish()})
	}
	return out
}
func (a *usageItemAccumulator) finish() api.UsageItemReport {
	out := a.out
	out.Summary = a.all.finish()
	out.Phases = usageGroups(a.phases)
	out.Roles = usageGroups(a.roles)
	out.Models = usageGroups(a.models)
	out.PhaseRoles = usageGroups(a.phaseRoles)
	return out
}
func (s *Store) Usage(ctx context.Context, task string, q api.UsageQuery) (api.UsageReport, error) {
	out := api.UsageReport{Version: api.UsageVersion, ProjectID: task, Items: []api.UsageItemReport{}}
	if !api.ValidID(task, "tsk") || (q.Item != "" && !api.ValidID(q.Item, "wi")) || (!q.From.IsZero() && !q.To.IsZero() && !q.From.Before(q.To)) {
		return out, api.ErrInvalid
	}
	if !q.From.IsZero() {
		v := q.From.UTC()
		out.From = &v
	}
	if !q.To.IsZero() {
		v := q.To.UTC()
		out.To = &v
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	prices, err := readUsagePrices(ctx, tx, task)
	if err != nil {
		return out, err
	}
	out.PriceRevision = prices.Revision
	coverageRows, err := tx.QueryContext(ctx, `SELECT agent_id,run_id,started_at,coverage FROM usage_runs WHERE task_id=?`, task)
	if err != nil {
		return out, err
	}
	for coverageRows.Next() {
		var c api.UsageCoverage
		if err = coverageRows.Scan(&c.AgentID, &c.RunID, &c.StartedAt, &c.State); err != nil {
			coverageRows.Close()
			return out, err
		}
		out.Coverage = append(out.Coverage, c)
	}
	err = coverageRows.Err()
	coverageRows.Close()
	if err != nil {
		return out, err
	}
	items := map[string]*usageItemAccumulator{}
	overhead := newUsageItem(task, "", "Project overhead")
	total := newUsageAccumulator()
	rows, err := tx.QueryContext(ctx, `SELECT id,title FROM work_items WHERE task_id=? ORDER BY seq`, task)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var id, title string
		if err = rows.Scan(&id, &title); err != nil {
			rows.Close()
			return out, err
		}
		if q.Item == "" || q.Item == id {
			items[task+"/"+id] = newUsageItem(task, id, title)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT projection FROM usage_turns ORDER BY task_id,agent_id,run_id,request_id`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			break
		}
		var p api.UsageProjection
		if err = json.Unmarshal([]byte(raw), &p); err != nil {
			break
		}
		t := p.Turn
		if (!q.From.IsZero() && t.At.Before(q.From)) || (!q.To.IsZero() && !t.At.Before(q.To)) {
			continue
		}
		key := p.AgentID + "/" + p.RunID + "/" + t.ID
		for _, share := range p.Shares {
			if share.TaskID != task || (q.Item != "" && share.ItemID != q.Item) {
				continue
			}
			total.add(key, t, share.Denominator, prices)
			if share.ItemID == "" {
				overhead.add(key, p, share.Denominator, prices)
			} else {
				itemKey := task + "/" + share.ItemID
				if items[itemKey] == nil {
					items[itemKey] = newUsageItem(task, share.ItemID, share.ItemID)
				}
				items[itemKey].add(key, p, share.Denominator, prices)
			}
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return out, err
	}
	out.Summary = total.finish()
	out.Overhead = overhead.finish()
	keys := make([]string, 0, len(items))
	for k := range items {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out.Items = append(out.Items, items[k].finish())
	}
	return out, nil
}

// ParseUsageTime enforces UTC half-open request-time filters, never observation time.
func ParseUsageTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	t, e := time.Parse(time.RFC3339Nano, value)
	if e != nil {
		return t, api.ErrInvalid
	}
	return t.UTC(), nil
}
