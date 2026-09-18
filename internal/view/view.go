// Package view 按观看者角色把案件投影裁剪为对外视图。
//
// 最小权限：客服（agent）看不到客户原始姓名/别名、命中中的原始姓名字段、
// 匹配规则明细（含名单姓名）以及调查笔记与证据摘要；这些字段以固定占位符遮蔽，
// 而不是简单省略，避免前端通过字段有无推断信息。
package view

import (
	"strings"

	"example.com/09181/q012/internal/domain"
	"example.com/09181/q012/internal/store"
)

// RedactedPlaceholder 统一的遮蔽占位符。
const RedactedPlaceholder = "***REDACTED***"

// HitView 命中视图；敏感字段随角色遮蔽。
type HitView struct {
	HitID       string             `json:"hit_id"`
	OriginCase  string             `json:"origin_case_id"` // 命中最初落入的案件，合并不改变它
	CaseID      string             `json:"case_id"`        // 当前根案件
	CustomerID  string             `json:"customer_id,omitempty"`
	ListID      string             `json:"list_id"`
	ListVersion int                `json:"list_version"`
	EntryID     string             `json:"entry_id"`
	MatchedName string             `json:"matched_name"`
	Score       float64            `json:"score"`
	Reasons     []domain.MatchReason `json:"reasons"`
	Origin      domain.HitOrigin   `json:"origin"`
	DuplicateOf string             `json:"duplicate_of,omitempty"`
	OccurredAt  string             `json:"occurred_at"`
	Redacted    bool               `json:"redacted"`
}

// NoteView 笔记视图；客服只见元数据。
type NoteView struct {
	AuthorID  string `json:"author_id"`
	CreatedAt string `json:"created_at"`
	Content   string `json:"content"`
}

// DispositionView 解除视图；客服看不到证据正文。
type DispositionView struct {
	ReviewerID        string `json:"reviewer_id"`
	Decision          string `json:"decision"`
	EvidenceSummary   string `json:"evidence_summary"`
	DuplicateOfCaseID string `json:"duplicate_of_case_id,omitempty"`
	CreatedAt         string `json:"created_at"`
}

// CaseView 案件台对外的案件全貌。
type CaseView struct {
	CaseID          string            `json:"case_id"`
	RootCaseID      string            `json:"root_case_id"`
	CustomerID      string            `json:"customer_id,omitempty"`
	Customer        *CustomerSnippet  `json:"customer,omitempty"`
	Status          string            `json:"status"`
	AggregationRule string            `json:"aggregation_rule"`
	Timezone        string            `json:"timezone,omitempty"`
	AssignedOfficer string            `json:"assigned_officer,omitempty"`
	RelatedCases    []string          `json:"related_cases"`
	Hits            []HitView         `json:"hits"`
	Notes           []NoteView        `json:"notes"`
	Disposition     *DispositionView  `json:"disposition,omitempty"`
	OpenedAt        string            `json:"opened_at"`
	ViewerRole      string            `json:"viewer_role"`
}

// CustomerSnippet 客户摘要；原始姓名对客服遮蔽。
type CustomerSnippet struct {
	CustomerID  string   `json:"customer_id"`
	LegalName   string   `json:"legal_name"`
	Aliases     []string `json:"aliases"`
	DateOfBirth string   `json:"date_of_birth"`
	Nationality string   `json:"nationality"`
}

// Builder 按角色生成视图。
type Builder struct {
	Role domain.Role
}

// ForRole 返回某角色的视图构造器。
func ForRole(r domain.Role) Builder { return Builder{Role: r} }

// CanSeeSensitiveData 是否可见原始姓名与调查材料。
func CanSeeSensitiveData(r domain.Role) bool {
	return r == domain.RoleOfficer || r == domain.RoleReviewer
}

// Case 把快照（及可选客户主数据）裁剪为案件视图。
func (b Builder) Case(snap store.Snapshot, customer *domain.Customer) CaseView {
	c := snap.Case
	v := CaseView{
		CaseID:          c.CaseID,
		RootCaseID:      snap.RootCaseID,
		CustomerID:      c.CustomerID,
		Status:          string(c.EffectiveStatus()),
		AggregationRule: c.AggregationRule,
		Timezone:        c.Timezone,
		AssignedOfficer: c.AssignedOfficer,
		RelatedCases:    append([]string{}, snap.RelatedCases...),
		OpenedAt:        c.OpenedAt.Format("2006-01-02T15:04:05Z07:00"),
		ViewerRole:      string(b.Role),
	}
	privileged := CanSeeSensitiveData(b.Role)

	for _, h := range snap.Hits {
		hv := HitView{
			HitID:       h.HitID,
			OriginCase:  h.CaseID,
			CaseID:      snap.RootCaseID,
			CustomerID:  h.CustomerID,
			ListID:      h.ListID,
			ListVersion: h.ListVersion,
			EntryID:     h.EntryID,
			MatchedName: h.MatchedName,
			Score:       h.Score,
			Reasons:     append([]domain.MatchReason{}, h.Reasons...),
			Origin:      h.Origin,
			DuplicateOf: h.DuplicateOf,
			OccurredAt:  h.OccurredAt.Format("2006-01-02T15:04:05Z07:00"),
		}
		if !privileged {
			hv.Redacted = true
			hv.MatchedName = RedactedPlaceholder
			// 规则代码（fuzzy_name 等）不含姓名可保留；明细可能内嵌名单姓名，须遮蔽。
			for i := range hv.Reasons {
				hv.Reasons[i].Detail = ""
			}
		}
		v.Hits = append(v.Hits, hv)
	}

	if privileged {
		for _, n := range snap.Notes {
			v.Notes = append(v.Notes, NoteView{
				AuthorID: n.AuthorID, Content: n.Content,
				CreatedAt: n.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
			})
		}
	}

	if d := snap.Disposition; d != nil {
		dv := &DispositionView{
			ReviewerID:        d.ReviewerID,
			Decision:          string(d.Decision),
			DuplicateOfCaseID: d.DuplicateOfCaseID,
			CreatedAt:         d.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		}
		if privileged {
			dv.EvidenceSummary = d.EvidenceSummary
		} else {
			dv.EvidenceSummary = RedactedPlaceholder
		}
		v.Disposition = dv
	}

	if customer != nil && customer.CustomerID != "" {
		snip := &CustomerSnippet{
			CustomerID:  customer.CustomerID,
			DateOfBirth: customer.DateOfBirth,
			Nationality: customer.Nationality,
		}
		if privileged {
			snip.LegalName = customer.LegalName
			snip.Aliases = append([]string{}, customer.Aliases...)
		} else {
			snip.LegalName = RedactedPlaceholder
			if len(customer.Aliases) > 0 {
				snip.Aliases = []string{RedactedPlaceholder}
			}
		}
		v.Customer = snip
	}
	return v
}

// MaskError 对低权限角色屏蔽可能含姓名的输入回显错误细节。
func MaskError(r domain.Role, msg string) string {
	if CanSeeSensitiveData(r) {
		return msg
	}
	if i := strings.IndexByte(msg, ':'); i > 0 {
		return msg[:i]
	}
	return msg
}
