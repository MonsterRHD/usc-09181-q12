package screening

import "sort"

// HitView 命中的对外视图。权限不足的客服看不到原始姓名：
// RawName 仅对 officer/reviewer 可见，agent 得到掩码。
type HitView struct {
	Hit
	RawNameVisible bool
}

// CaseView 案件视图，按角色掩码。
type CaseView struct {
	Case
	Hits []HitView
}

// View 只读访问门面，所有读取都来自事件重放的投影，因此审计口径与写入一致。
type View struct {
	proj  *Projection
	authz Authorizer
}

func NewView(proj *Projection, authz Authorizer) *View {
	return &View{proj: proj, authz: authz}
}

// Case 返回案件视图；agent 看到的命中姓名被掩码。
func (v *View) Case(actor, caseID string) (*CaseView, error) {
	c := v.proj.Cases[caseID]
	if c == nil {
		return nil, ErrNotFound
	}
	canSeeName := false
	switch v.authz.RoleOf(actor) {
	case RoleOfficer, RoleReviewer:
		canSeeName = true
	}
	cp := *c
	cp.HitIDs = append([]string{}, c.HitIDs...)
	cp.SourceCases = append([]string{}, c.SourceCases...)
	cp.Rules = append([]string{}, c.Rules...)
	cv := &CaseView{Case: cp}
	for _, hid := range c.HitIDs {
		h := v.proj.Hits[hid]
		hv := HitView{Hit: h, RawNameVisible: canSeeName}
		if !canSeeName {
			hv.RawName = MaskName(h.RawName)
		}
		cv.Hits = append(cv.Hits, hv)
	}
	sort.SliceStable(cv.Hits, func(i, j int) bool { return cv.Hits[i].ID < cv.Hits[j].ID })
	return cv, nil
}

// Cases 列出全部案件（按 ID 排序）。
func (v *View) Cases(actor string) ([]*CaseView, error) {
	ids := make([]string, 0, len(v.proj.Cases))
	for id := range v.proj.Cases {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*CaseView, 0, len(ids))
	for _, id := range ids {
		cv, err := v.Case(actor, id)
		if err != nil {
			return nil, err
		}
		out = append(out, cv)
	}
	return out, nil
}

// AuditTrail 返回案件相关事件（按序号顺序），供复核/审计重放检查。
func (v *View) AuditTrail(caseID string) []Event {
	hitIDs := map[string]bool{}
	if c := v.proj.Cases[caseID]; c != nil {
		for _, h := range c.HitIDs {
			hitIDs[h] = true
		}
	}
	var out []Event
	for _, e := range v.proj.Events {
		if eventTouchesCase(e, caseID, hitIDs) {
			out = append(out, e)
		}
	}
	return out
}

func eventTouchesCase(e Event, caseID string, hitIDs map[string]bool) bool {
	if e.CaseID == caseID || e.IntoCase == caseID {
		return true
	}
	for _, c := range e.Cases {
		if c == caseID {
			return true
		}
	}
	for _, g := range e.Groups {
		if g.CaseID == caseID {
			return true
		}
	}
	return false
}
