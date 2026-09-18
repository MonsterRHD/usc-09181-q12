package screening

import (
	"errors"
	"fmt"
	"time"
)

// Role 角色。客服权限不足，看不到命中的原始姓名。
type Role string

const (
	RoleAgent    Role = "agent"    // 客服：只可见掩码姓名
	RoleOfficer  Role = "officer"  // 合规官：可看原始姓名、可调查
	RoleReviewer Role = "reviewer" // 复核人：可执行解除
)

type Authorizer interface{ RoleOf(actor string) Role }

// MapAuthorizer 测试/简单部署用的静态授权表。
type MapAuthorizer map[string]Role

func (m MapAuthorizer) RoleOf(actor string) Role {
	if r, ok := m[actor]; ok {
		return r
	}
	return RoleAgent
}

var (
	ErrNotFound       = errors.New("screening: not found")
	ErrForbidden      = errors.New("screening: operation forbidden for role")
	ErrCaseBoundary   = errors.New("screening: case boundary violated")
	ErrNeedsReviewer  = errors.New("screening: clearance requires reviewer and evidence summary")
	ErrStillActive    = errors.New("screening: case still has active list hits; cannot clear retroactively")
	ErrSuspended      = errors.New("screening: case is suspended")
	ErrInvalidInput   = errors.New("screening: invalid input")
)

// Service 案件台应用服务。所有写操作：重放当前事件 -> 校验 -> 以乐观锁原子追加事件。
// 并发命令冲突时自动有限次重试，因此"撤回与重新发布并发处理"不会丢失更新。
type Service struct {
	store EventStore
	authz Authorizer
	now   func() time.Time
}

func NewService(store EventStore, authz Authorizer) *Service {
	return &Service{store: store, authz: authz, now: func() time.Time { return time.Now().UTC() }}
}

// WithClock 注入时钟（测试跨时区截止/故障恢复顺序）。
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

func (s *Service) replay() (*Projection, error) { return LoadReplay(s.store) }

// 由于内存/文件存储的乐观锁方法不在 EventStore 接口上，这里用类型断言。
type optimisticStore interface {
	AppendExpect(expectedSeq int, events []Event) (int, int, error)
}

func (s *Service) memAppend(p *Projection, evs []Event) (int, int, error) {
	if os, ok := s.store.(optimisticStore); ok {
		return os.AppendExpect(p.Seq, evs)
	}
	return s.store.Append(evs...)
}

// retry 重放-提交，冲突时重试。
func (s *Service) retry(fn func(p *Projection) ([]Event, error)) error {
	for attempt := 0; attempt < 10; attempt++ {
		p, err := s.replay()
		if err != nil {
			return err
		}
		evs, err := fn(p)
		if err != nil {
			return err
		}
		if len(evs) == 0 {
			return nil
		}
		_, _, err = s.memAppend(p, evs)
		if errors.Is(err, ErrConflict) {
			continue
		}
		return err
	}
	return ErrConflict
}

func (s *Service) newEvent(typ, actor string) Event {
	return Event{Type: typ, Actor: actor, OccurredAt: s.now()}
}

func (s *Service) require(actor string, roles ...Role) error {
	r := s.authz.RoleOf(actor)
	for _, allowed := range roles {
		if r == allowed {
			return nil
		}
	}
	return fmt.Errorf("%w: %s 需要 %v，实际 %s", ErrForbidden, actor, roles, r)
}

// ---- 主数据 / 名单 ----

func (s *Service) ImportCustomers(actor string, customers []Customer) error {
	if err := s.require(actor, RoleOfficer, RoleReviewer); err != nil {
		return err
	}
	if len(customers) == 0 {
		return nil
	}
	e := s.newEvent(EvCustomersUpserted, actor)
	e.Customers = customers
	_, _, err := s.store.Append(e)
	return err
}

func (s *Service) ImportListEntries(actor string, entries []ListEntry) error {
	if err := s.require(actor, RoleOfficer, RoleReviewer); err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	e := s.newEvent(EvListEntriesImported, actor)
	e.ListEntries = entries
	_, _, err := s.store.Append(e)
	return err
}

// WithdrawListEntry 名单撤回（回溯）：版本置为失效。受影响案件不会被静默改写结论，
// 需由复核人通过 ResolveRetroactive 引用证据后解除。
func (s *Service) WithdrawListEntry(actor, listID, withdrawnVersion string) error {
	if err := s.require(actor, RoleOfficer, RoleReviewer); err != nil {
		return err
	}
	return s.retry(func(p *Projection) ([]Event, error) {
		if _, ok := p.ListEntries[listID]; !ok {
			return nil, fmt.Errorf("%w: list entry %s", ErrNotFound, listID)
		}
		e := s.newEvent(EvListWithdrawn, actor)
		e.ListID = listID
		e.NewVersion = withdrawnVersion
		return []Event{e}, nil
	})
}

// RepublishListEntry 名单撤回后以新版本重新发布（可能修正条目内容）。
func (s *Service) RepublishListEntry(actor string, entry ListEntry) error {
	if err := s.require(actor, RoleOfficer, RoleReviewer); err != nil {
		return err
	}
	if entry.Version == "" {
		return fmt.Errorf("%w: 重新发布必须带新版本号", ErrInvalidInput)
	}
	entry.Active = true
	return s.retry(func(p *Projection) ([]Event, error) {
		if _, ok := p.ListEntries[entry.ID]; !ok {
			return nil, fmt.Errorf("%w: list entry %s", ErrNotFound, entry.ID)
		}
		e := s.newEvent(EvListRepublished, actor)
		e.ListID = entry.ID
		e.NewVersion = entry.Version
		// 条目修正内容随导入事件携带（同一追加流中先更新再标记重发）。
		imp := s.newEvent(EvListEntriesImported, actor)
		imp.ListEntries = []ListEntry{entry}
		return []Event{imp, e}, nil
	})
}

// ---- 命中接入与聚合 ----

// IngestBatch 接收一批筛查命中，按可解释规则聚合为新案件。
// 已归属案件的命中（同 ID）不会重复建案；不同客户 ID 的命中即使姓名相似也被边界隔开，
// 计划中的 KeptApart 会作为规则说明保留在事件中以便审计。
func (s *Service) IngestBatch(actor, batchID string, hits []Hit) (newCaseIDs []string, plan AggregationPlan, err error) {
	if err = s.require(actor, RoleOfficer, RoleReviewer); err != nil {
		return nil, AggregationPlan{}, err
	}
	if batchID == "" || len(hits) == 0 {
		return nil, AggregationPlan{}, fmt.Errorf("%w: batchID 与 hits 不能为空", ErrInvalidInput)
	}
	err = s.retry(func(p *Projection) ([]Event, error) {
		newCaseIDs = nil
		var fresh []Hit
		for _, h := range hits {
			if _, exists := p.Hits[h.ID]; exists {
				continue // 重复投递的同一命中不重复建案
			}
			fresh = append(fresh, h)
		}
		if len(fresh) == 0 {
			return nil, nil
		}
		var customers []Customer
		for _, c := range p.Customers {
			customers = append(customers, c)
		}
		var entries []ListEntry
		for _, le := range p.ListEntries {
			entries = append(entries, le)
		}
		matcher := NewMatcher(customers, entries)
		plan = matcher.Plan(fresh)

		ev := s.newEvent(EvHitsIngested, actor)
		ev.BatchID = batchID
		ev.Hits = fresh

		agg := s.newEvent(EvCasesAggregated, actor)
		agg.BatchID = batchID
		for i, g := range plan.Groups {
			cid := fmt.Sprintf("CASE-%d-%d", p.Seq+2, i+1)
			newCaseIDs = append(newCaseIDs, cid)
			cg := CaseGroup{CaseID: cid, HitIDs: g}
			// 客户 ID 与规则说明：组内命中共享同一客户则记录；规则来自匹配边。
			custID := ""
			for _, hid := range g {
				for _, h := range fresh {
					if h.ID == hid {
						if custID == "" {
							custID = h.CustomerID
						} else if custID != h.CustomerID {
							custID = ""
						}
					}
				}
			}
			cg.CustomerID = custID
			cg.Rules = rulesForGroup(g, plan)
			agg.Groups = append(agg.Groups, cg)
		}
		return []Event{ev, agg}, nil
	})
	if err != nil {
		return nil, AggregationPlan{}, err
	}
	return newCaseIDs, plan, nil
}

// rulesForGroup 取组内命中涉及的匹配边规则（可解释性）。
func rulesForGroup(group []string, plan AggregationPlan) []string {
	in := map[string]bool{}
	for _, id := range group {
		in[id] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range plan.Edges {
		if in[e.A] && in[e.B] && !seen[e.Rule] {
			seen[e.Rule] = true
			out = append(out, e.Rule+": "+e.Detail)
		}
	}
	return out
}

// ---- 案件操作 ----

// MergeCases 手工/规则并案：只能把来源案件的关系与命中追加进目标案件，
// 原命中与来源案件都保留。不同客户的案件之间禁止合并（案件边界）。
func (s *Service) MergeCases(actor, intoCase string, sourceCases []string, rule string) error {
	if err := s.require(actor, RoleOfficer, RoleReviewer); err != nil {
		return err
	}
	if intoCase == "" || len(sourceCases) == 0 {
		return fmt.Errorf("%w: 目标案件与来源案件不能为空", ErrInvalidInput)
	}
	return s.retry(func(p *Projection) ([]Event, error) {
		target := p.Cases[intoCase]
		if target == nil {
			return nil, fmt.Errorf("%w: case %s", ErrNotFound, intoCase)
		}
		for _, sid := range sourceCases {
			src := p.Cases[sid]
			if src == nil {
				return nil, fmt.Errorf("%w: case %s", ErrNotFound, sid)
			}
			if src.CustomerID != "" && target.CustomerID != "" && src.CustomerID != target.CustomerID {
				return nil, fmt.Errorf("%w: 案件 %s(%s) 与 %s(%s) 分属不同客户，禁止合并",
					ErrCaseBoundary, sid, src.CustomerID, intoCase, target.CustomerID)
			}
		}
		e := s.newEvent(EvCasesMerged, actor)
		e.IntoCase = intoCase
		e.Cases = sourceCases
		e.Rule = rule
		return []Event{e}, nil
	})
}

// AssignCase 分配案件给合规官，同时登记提醒与升级截止时刻（绝对时刻，调用方完成时区换算）。
func (s *Service) AssignCase(actor, caseID, officer string, reminderDue, escalationDue time.Time) error {
	if err := s.require(actor, RoleOfficer, RoleReviewer); err != nil {
		return err
	}
	return s.retry(func(p *Projection) ([]Event, error) {
		c := p.Cases[caseID]
		if c == nil {
			return nil, fmt.Errorf("%w: case %s", ErrNotFound, caseID)
		}
		var evs []Event
		e := s.newEvent(EvCaseAssigned, actor)
		e.CaseID = caseID
		e.Officer = officer
		evs = append(evs, e)
		if !reminderDue.IsZero() {
			r := s.newEvent(EvReminderScheduled, actor)
			r.CaseID, r.HitID, r.Kind, r.DueAt = caseID, caseID+":reminder", "reminder", reminderDue.UTC()
			evs = append(evs, r)
		}
		if !escalationDue.IsZero() {
			r := s.newEvent(EvReminderScheduled, actor)
			r.CaseID, r.HitID, r.Kind, r.DueAt = caseID, ":escalation", "escalation", escalationDue.UTC()
			evs = append(evs, r)
		}
		return evs, nil
	})
}

// SuspendCase 暂停案件（等待名单方澄清等）。暂停期间不推动提醒；
// 恢复时按暂停时长顺延，升级时钟不因为暂停被消耗。
func (s *Service) SuspendCase(actor, caseID, reason string) (time.Time, error) {
	if err := s.require(actor, RoleOfficer, RoleReviewer); err != nil {
		return time.Time{}, err
	}
	err := s.retry(func(p *Projection) ([]Event, error) {
		c := p.Cases[caseID]
		if c == nil {
			return nil, fmt.Errorf("%w: case %s", ErrNotFound, caseID)
		}
		if c.Suspended {
			return nil, ErrSuspended
		}
		e := s.newEvent(EvCaseSuspended, actor)
		e.CaseID, e.Reason = caseID, reason
		return []Event{e}, nil
	})
	return s.now(), err
}

// ResumeCase 恢复案件；未触发提醒按暂停时长顺延，升级时钟不被暂停消耗。
func (s *Service) ResumeCase(actor, caseID string, suspendedAt time.Time) error {
	if err := s.require(actor, RoleOfficer, RoleReviewer); err != nil {
		return err
	}
	resumedAt := s.now()
	return s.retry(func(p *Projection) ([]Event, error) {
		c := p.Cases[caseID]
		if c == nil {
			return nil, fmt.Errorf("%w: case %s", ErrNotFound, caseID)
		}
		if !c.Suspended {
			return nil, fmt.Errorf("%w: 案件未暂停", ErrInvalidInput)
		}
		ev := s.newEvent(EvCaseResumed, actor)
		ev.CaseID = caseID
		delta := resumedAt.Sub(suspendedAt)
		var evs []Event
		evs = append(evs, ev)
		if delta > 0 {
			d := s.newEvent(EvRemindersDeferred, actor)
			d.CaseID, d.Delta = caseID, delta
			evs = append(evs, d)
		}
		return evs, nil
	})
}

// RecordDisposition 记录处置结论。任何"解除"（误报/重复/名单回溯解除）
// 都必须引用复核人并附证据摘要；真阳性同样要求复核人。
func (s *Service) RecordDisposition(actor, caseID string, disp Disposition, reviewer, evidence string) error {
	if err := s.require(actor, RoleOfficer, RoleReviewer); err != nil {
		return err
	}
	isRelease := disp == DispFalsePositive || disp == DispDuplicate ||
		disp == DispRetroactiveClear || disp == DispTruePositive
	switch disp {
	case DispPending, DispFalsePositive, DispTruePositive, DispDuplicate, DispRetroactiveClear, DispEscalated:
	default:
		return fmt.Errorf("%w: 未知处置结论 %q", ErrInvalidInput, disp)
	}
	if isRelease && (reviewer == "" || evidence == "") {
		return fmt.Errorf("%w: %s", ErrNeedsReviewer, disp)
	}
	if isRelease && s.authz.RoleOf(reviewer) != RoleReviewer {
		return fmt.Errorf("%w: 复核人 %s 缺少 reviewer 角色", ErrForbidden, reviewer)
	}
	return s.retry(func(p *Projection) ([]Event, error) {
		c := p.Cases[caseID]
		if c == nil {
			return nil, fmt.Errorf("%w: case %s", ErrNotFound, caseID)
		}
		if c.Suspended {
			return nil, ErrSuspended
		}
		e := s.newEvent(EvDisposition, actor)
		e.CaseID, e.Reason, e.Reviewer, e.Evidence = caseID, string(disp), reviewer, evidence
		return []Event{e}, nil
	})
}

// ResolveRetroactive 名单回溯解除：仅当案件内全部命中所依据的名单条目均已撤回时允许，
// 状态置 LIST_WITHDRAWN_CLEARED，并引用复核人与证据。
func (s *Service) ResolveRetroactive(actor, caseID, reviewer, evidence string) error {
	if err := s.require(actor, RoleOfficer, RoleReviewer); err != nil {
		return err
	}
	if reviewer == "" || evidence == "" {
		return fmt.Errorf("%w: %s", ErrNeedsReviewer, DispRetroactiveClear)
	}
	if s.authz.RoleOf(reviewer) != RoleReviewer {
		return fmt.Errorf("%w: 复核人 %s 缺少 reviewer 角色", ErrForbidden, reviewer)
	}
	return s.retry(func(p *Projection) ([]Event, error) {
		c := p.Cases[caseID]
		if c == nil {
			return nil, fmt.Errorf("%w: case %s", ErrNotFound, caseID)
		}
		for _, hid := range c.HitIDs {
			h, ok := p.Hits[hid]
			if !ok {
				continue
			}
			le, ok := p.ListEntries[h.ListEntryID]
			if ok && le.Active {
				return nil, fmt.Errorf("%w: 命中 %s 依据的名单 %s 仍有效", ErrStillActive, hid, h.ListEntryID)
			}
		}
		e := s.newEvent(EvDisposition, actor)
		e.CaseID, e.Reason, e.Reviewer, e.Evidence = caseID, string(DispRetroactiveClear), reviewer, evidence
		return []Event{e}, nil
	})
}

// DetachHit 把错聚到案件的命中材料分离出去（追加式：命中与历史不删除，只记录分离关系），
// 必须引用复核人与证据。
func (s *Service) DetachHit(actor, caseID, hitID, reviewer, evidence string) error {
	if err := s.require(actor, RoleOfficer, RoleReviewer); err != nil {
		return err
	}
	if reviewer == "" || evidence == "" {
		return fmt.Errorf("%w: 分离命中", ErrNeedsReviewer)
	}
	if s.authz.RoleOf(reviewer) != RoleReviewer {
		return fmt.Errorf("%w: 复核人 %s 缺少 reviewer 角色", ErrForbidden, reviewer)
	}
	return s.retry(func(p *Projection) ([]Event, error) {
		c := p.Cases[caseID]
		if c == nil {
			return nil, fmt.Errorf("%w: case %s", ErrNotFound, caseID)
		}
		if !contains(c.HitIDs, hitID) {
			return nil, fmt.Errorf("%w: hit %s 不在案件 %s 中", ErrNotFound, hitID, caseID)
		}
		e := s.newEvent(EvUnmatchedHitDetached, actor)
		e.CaseID, e.HitID, e.Reviewer, e.Evidence = caseID, hitID, reviewer, evidence
		return []Event{e}, nil
	})
}

func (s *Service) AddNote(actor, caseID, noteID, content string) error {
	if err := s.require(actor, RoleOfficer, RoleReviewer); err != nil {
		return err
	}
	return s.retry(func(p *Projection) ([]Event, error) {
		if _, ok := p.Cases[caseID]; !ok {
			return nil, fmt.Errorf("%w: case %s", ErrNotFound, caseID)
		}
		if _, dup := p.Notes[noteID]; dup {
			return nil, fmt.Errorf("%w: note %s 已存在", ErrInvalidInput, noteID)
		}
		e := s.newEvent(EvNoteAdded, actor)
		e.CaseID, e.NoteID, e.Evidence = caseID, noteID, content
		return []Event{e}, nil
	})
}

// PushDue 推动所有截止时刻 <= asOf 的提醒/升级，严格按发生顺序产出触发事件。
// 故障恢复后重复调用是安全的：已触发的提醒不会再次推动（不重不漏）。
// 返回本次推动的提醒键，按发生顺序。
func (s *Service) PushDue(asOf time.Time) ([]DueReminder, error) {
	var fired []DueReminder
	err := s.retry(func(p *Projection) ([]Event, error) {
		fired = nil
		due := p.DueItems(asOf.UTC())
		if len(due) == 0 {
			return nil, nil
		}
		evs := make([]Event, 0, len(due))
		for _, d := range due {
			typ := EvReminderFired
			if d.Kind == "escalation" {
				typ = EvEscalationFired
			}
			e := s.newEvent(typ, "system")
			e.CaseID, e.HitID = d.CaseID, d.Key
			evs = append(evs, e)
			fired = append(fired, d)
		}
		return evs, nil
	})
	return fired, err
}
