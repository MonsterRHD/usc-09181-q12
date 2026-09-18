package screening

import (
	"sort"
	"time"
)

// Projection 是事件流的读模型：从零开始 Apply 全部事件即可重建。
// 故障恢复或审计复核时，重新加载事件流并重放，得到与故障前一致的状态。
type Projection struct {
	Seq         int
	Hits        map[string]Hit
	Cases       map[string]*Case
	HitCase     map[string]string // hitID -> 当前所属案件
	ListEntries map[string]ListEntry
	Notes       map[string]InvestigationNote
	Customers   map[string]Customer
	Events      []Event

	scheduledDue map[string]DueReminder // key -> 提醒（截止时刻统一为 UTC）
	reminders    []DueReminder          // 已触发记录，按发生顺序

	// 追加式分离记录：detach 不删除命中，只记录分离关系
	Detached map[string][]string // caseID -> 已分离 hitID
}

func NewProjection() *Projection {
	return &Projection{
		Hits:         map[string]Hit{},
		Cases:        map[string]*Case{},
		HitCase:      map[string]string{},
		ListEntries:  map[string]ListEntry{},
		Notes:        map[string]InvestigationNote{},
		Customers:    map[string]Customer{},
		scheduledDue: map[string]DueReminder{},
		Detached:     map[string][]string{},
	}
}

// LoadReplay 从事件存储读取全部事件并重放，同时校验哈希链。
func LoadReplay(store EventStore) (*Projection, error) {
	events, err := store.Events()
	if err != nil {
		return nil, err
	}
	if err := VerifyChain(events); err != nil {
		return nil, err
	}
	p := NewProjection()
	for _, e := range events {
		p.Apply(e)
	}
	return p, nil
}

func (p *Projection) Apply(e Event) {
	p.Events = append(p.Events, e)
	p.Seq = e.Seq
	switch e.Type {

	case EvCustomersUpserted:
		for _, c := range e.Customers {
			if old, ok := p.Customers[c.ID]; !ok || !c.UpdatedAt.Before(old.UpdatedAt) {
				p.Customers[c.ID] = c
			}
		}

	case EvListEntriesImported:
		for _, le := range e.ListEntries {
			p.ListEntries[le.ID] = le
		}

	case EvHitsIngested:
		for _, h := range e.Hits {
			p.Hits[h.ID] = h
		}

	case EvCasesAggregated:
		for _, g := range e.Groups {
			if _, exists := p.Cases[g.CaseID]; exists {
				continue
			}
			c := &Case{
				ID:           g.CaseID,
				HitIDs:       append([]string{}, g.HitIDs...),
				CustomerID:   g.CustomerID,
				ListVersions: map[string]string{},
				Status:       DispPending,
				Rules:        append([]string{}, g.Rules...),
				CreatedAt:    e.OccurredAt,
				UpdatedAt:    e.OccurredAt,
			}
			if c.CustomerID != "" {
				if cust, ok := p.Customers[c.CustomerID]; ok {
					c.CustomerName = cust.LegalName
				}
			}
			for _, hid := range c.HitIDs {
				p.HitCase[hid] = c.ID
				if h, ok := p.Hits[hid]; ok {
					c.ListVersions[h.ListEntryID] = h.ListVersion
				}
			}
			p.Cases[c.ID] = c
		}

	case EvCasesMerged:
		target := p.Cases[e.IntoCase]
		if target == nil {
			return
		}
		for _, srcID := range e.Cases {
			src := p.Cases[srcID]
			if src == nil || srcID == e.IntoCase {
				continue
			}
			// 合并只能追加关系：来源案件保留（标记 MERGED），其命中追加进目标案件，
			// 原命中材料绝不被覆盖或删除。
			target.SourceCases = append(target.SourceCases, srcID)
			for _, hid := range src.HitIDs {
				if !contains(target.HitIDs, hid) {
					target.HitIDs = append(target.HitIDs, hid)
				}
				p.HitCase[hid] = target.ID
				if h, ok := p.Hits[hid]; ok {
					target.ListVersions[h.ListEntryID] = h.ListVersion
				}
			}
			if e.Rule != "" && !contains(target.Rules, e.Rule) {
				target.Rules = append(target.Rules, e.Rule)
			}
			target.UpdatedAt = e.OccurredAt
			src.Status = DispMerged
			src.UpdatedAt = e.OccurredAt
		}

	case EvCaseAssigned:
		if c := p.Cases[e.CaseID]; c != nil {
			c.Assignee = e.Officer
			c.UpdatedAt = e.OccurredAt
		}

	case EvReminderScheduled:
		key := e.HitID
		p.scheduledDue[key] = DueReminder{
			Key: key, CaseID: e.CaseID, OccurredAt: e.DueAt, Kind: e.Kind,
		}

	case EvRemindersDeferred:
		// 暂停占用的时间在恢复时整体顺延：未触发的提醒截止时刻 += Delta。
		for k, d := range p.scheduledDue {
			if d.CaseID == e.CaseID && !d.Fired {
				d.OccurredAt = d.OccurredAt.Add(e.Delta)
				p.scheduledDue[k] = d
			}
		}

	case EvEscalationFired, EvReminderFired:
		k := e.HitID
		if d, ok := p.scheduledDue[k]; ok {
			d.Fired = true
			p.scheduledDue[k] = d
			p.reminders = append(p.reminders, d)
			if c := p.Cases[d.CaseID]; c != nil && e.Type == EvEscalationFired && c.Status == DispPending {
				c.Status = DispEscalated
				c.UpdatedAt = e.OccurredAt
			}
		}

	case EvDisposition:
		c := p.Cases[e.CaseID]
		if c == nil {
			return
		}
		c.Status = Disposition(e.Reason) // Reason 承载处置结论
		c.Reviewer = e.Reviewer
		c.Evidence = e.Evidence
		c.UpdatedAt = e.OccurredAt

	case EvListWithdrawn:
		if le, ok := p.ListEntries[e.ListID]; ok {
			le.Active = false
			le.Version = e.NewVersion
			p.ListEntries[e.ListID] = le
		}

	case EvListRepublished:
		le := p.ListEntries[e.ListID]
		le.ID = e.ListID
		le.Version = e.NewVersion
		le.Active = true
		p.ListEntries[e.ListID] = le

	case EvCaseSuspended:
		if c := p.Cases[e.CaseID]; c != nil {
			c.Suspended = true
			c.SuspendReason = e.Reason
			c.UpdatedAt = e.OccurredAt
		}

	case EvCaseResumed:
		if c := p.Cases[e.CaseID]; c != nil {
			c.Suspended = false
			c.SuspendReason = ""
			c.UpdatedAt = e.OccurredAt
		}

	case EvNoteAdded:
		p.Notes[e.NoteID] = InvestigationNote{
			ID: e.NoteID, CaseID: e.CaseID, Author: e.Actor,
			Content: e.Evidence, CreatedAt: e.OccurredAt,
		}

	case EvUnmatchedHitDetached:
		// 追加式分离：命中仍保留在事件流与 p.Hits 中，仅记录分离关系并从活动命中列表移除。
		c := p.Cases[e.CaseID]
		if c == nil {
			return
		}
		kept := c.HitIDs[:0:0]
		for _, hid := range c.HitIDs {
			if hid != e.HitID {
				kept = append(kept, hid)
			}
		}
		c.HitIDs = kept
		if !contains(p.Detached[c.ID], e.HitID) {
			p.Detached[c.ID] = append(p.Detached[c.ID], e.HitID)
		}
		c.Reviewer = e.Reviewer
		c.Evidence = e.Evidence
		c.UpdatedAt = e.OccurredAt
	}
}

// DueItems 返回尚未触发、截止时刻 <= asOf 的提醒/升级，按发生顺序（OccurredAt，再按键）排列。
// 暂停中的案件不产生提醒；已结案后不再提醒。跨时区截止时刻在入队时已换算为 UTC，
// 因此恢复后顺序与原截止顺序一致。
func (p *Projection) DueItems(asOf time.Time) []DueReminder {
	var out []DueReminder
	for _, d := range p.scheduledDue {
		if d.Fired || d.OccurredAt.After(asOf) {
			continue
		}
		if c := p.Cases[d.CaseID]; c != nil {
			if c.Suspended || isClosed(c.Status) {
				continue
			}
		}
		out = append(out, d)
	}
	sortReminders(out)
	return out
}

// FiredReminders 已实际推动过的提醒（审计用），按发生顺序。
func (p *Projection) FiredReminders() []DueReminder {
	out := append([]DueReminder(nil), p.reminders...)
	sortReminders(out)
	return out
}

func sortReminders(out []DueReminder) {
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].OccurredAt.Equal(out[j].OccurredAt) {
			return out[i].Key < out[j].Key
		}
		return out[i].OccurredAt.Before(out[j].OccurredAt)
	})
}

func isClosed(s Disposition) bool {
	switch s {
	case DispFalsePositive, DispTruePositive, DispDuplicate, DispRetroactiveClear, DispMerged:
		return true
	}
	return false
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
