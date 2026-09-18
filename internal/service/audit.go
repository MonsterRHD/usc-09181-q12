package service

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"example.com/09181/q012/internal/domain"
	"example.com/09181/q012/internal/store"
)

// AuditEvent 审计时间线上的一条记录。
type AuditEvent struct {
	Seq       int64     `json:"seq"`
	Type      string    `json:"type"`
	At        time.Time `json:"at"`
	Summary   string    `json:"summary"`    // 人读摘要
	CaseID    string    `json:"case_id,omitempty"`
	Reviewer  string    `json:"reviewer,omitempty"`
	Evidence  string    `json:"evidence,omitempty"`
}

// CaseBoundary 描述一个案件根及其合并树边界，用于核对「合并只追加关系」。
type CaseBoundary struct {
	RootCaseID     string   `json:"root_case_id"`
	Status         string   `json:"status"`
	AbsorbedCases  []string `json:"absorbed_cases"`
	RelatedCases   []string `json:"related_cases"`
	RootHitIDs     []string `json:"root_hit_ids"`     // 原始命中仍记录在根案件
	AbsorbedHitIDs []string `json:"absorbed_hit_ids"` // 被吸收案件的原始命中保留原处
}

// AuditReport 合规复核的审计重放结论。
type AuditReport struct {
	LastSeq       int64        `json:"last_seq"`
	IntegrityOK   bool         `json:"integrity_ok"`
	Events        []AuditEvent `json:"events"`
	CaseBoundaries []CaseBoundary `json:"case_boundaries"`
	// DispositionsWithoutAttribution 为不合规解除（正常流程下应为空）。
	DispositionsWithoutAttribution []string `json:"dispositions_without_attribution"`
}

// AuditReplay 不依赖当前内存状态，直接从事件日志重新走一遍，
// 校验哈希链完整性、解除引用完整性，并产出案件边界报告。
func (s *Service) AuditReplay() (AuditReport, error) {
	rep := AuditReport{IntegrityOK: true}

	// 独立投影重放，保证审计结论只来源于日志。
	fresh := store.NewProjection()
	err := s.store.Replay(func(seq int64, typ string, data any) error {
		fresh.Apply(typ, data)
		rep.Events = append(rep.Events, summarize(seq, typ, data))
		rep.LastSeq = seq
		if typ == "disposition.recorded" {
			d := data.(*domain.DispositionRecorded).Disposition
			if strings.TrimSpace(d.ReviewerID) == "" || strings.TrimSpace(d.EvidenceSummary) == "" {
				rep.DispositionsWithoutAttribution = append(
					rep.DispositionsWithoutAttribution, d.CaseID)
			}
		}
		return nil
	})
	if err != nil {
		rep.IntegrityOK = false
		return rep, err
	}

	for _, c := range fresh.Cases() {
		snap, ok := fresh.CaseSnapshot(c.CaseID)
		if !ok {
			continue
		}
		b := CaseBoundary{
			RootCaseID:   c.CaseID,
			Status:       string(c.Status),
			RelatedCases: append([]string{}, snap.RelatedCases...),
			RootHitIDs:   append([]string{}, c.HitIDs...),
		}
		for _, related := range snap.RelatedCases {
			if related == c.CaseID {
				continue
			}
			b.AbsorbedCases = append(b.AbsorbedCases, related)
			// 必须取被吸收案件自身（不解析到根），才能核对原命中未被搬移。
			if child, ok := fresh.CaseOwn(related); ok {
				b.AbsorbedHitIDs = append(b.AbsorbedHitIDs, child.HitIDs...)
			}
		}
		sort.Strings(b.AbsorbedCases)
		sort.Strings(b.AbsorbedHitIDs)
		if c.Paused {
			b.Status = string(domain.StatusPaused)
		}
		rep.CaseBoundaries = append(rep.CaseBoundaries, b)
	}
	sort.Slice(rep.CaseBoundaries, func(i, j int) bool {
		return rep.CaseBoundaries[i].RootCaseID < rep.CaseBoundaries[j].RootCaseID
	})
	return rep, nil
}

func summarize(seq int64, typ string, data any) AuditEvent {
	e := AuditEvent{Seq: seq, Type: typ}
	switch v := data.(type) {
	case *domain.CustomerUpserted:
		e.At = v.At
		e.Summary = "customer upserted: " + v.Customer.CustomerID
	case *domain.OfficerProvisioned:
		e.At = v.At
		e.Summary = fmt.Sprintf("officer provisioned: %s (%s, %s)", v.Officer.OfficerID, v.Officer.Name, v.Officer.Role)
	case *domain.ListPublished:
		e.At = v.PublishedAt
		e.Summary = fmt.Sprintf("list published: %s v%d (%d entries)", v.ListID, v.Version, len(v.Entries))
	case *domain.ListWithdrawn:
		e.At = v.WithdrawnAt
		e.Summary = fmt.Sprintf("list withdrawn: %s v%d", v.ListID, v.Version)
	case *domain.ListRepublished:
		e.At = v.At
		e.Summary = fmt.Sprintf("list republished: %s v%d based on v%d (%d entries)", v.ListID, v.Version, v.BaseVersion, len(v.Entries))
	case *domain.HitRecorded:
		e.At = v.Hit.OccurredAt
		e.CaseID = v.Hit.CaseID
		e.Summary = fmt.Sprintf("hit recorded: %s customer=%s entry=%s v%d score=%.2f origin=%s%s",
			v.Hit.HitID, dash(v.Hit.CustomerID), v.Hit.EntryID, v.Hit.ListVersion,
			v.Hit.Score, v.Hit.Origin, dupSuffix(v.Hit.DuplicateOf))
	case *domain.CaseOpened:
		e.At = v.OpenedAt
		e.CaseID = v.CaseID
		e.Summary = fmt.Sprintf("case opened: %s rule=%s hits=%v retroactive=%t",
			v.CaseID, v.AggregationRule, v.HitIDs, v.Retroactive)
	case *domain.HitsLinked:
		e.At = v.At
		e.CaseID = v.CaseID
		e.Summary = fmt.Sprintf("hits linked to %s: %v reason=%s", v.CaseID, v.HitIDs, v.Reason)
	case *domain.CasesMerged:
		e.At = v.At
		e.CaseID = v.SurvivorCaseID
		e.Summary = fmt.Sprintf("merge: %s absorbed into %s by %s", v.AbsorbedCaseID, v.SurvivorCaseID, v.OfficerID)
	case *domain.CaseAssigned:
		e.At = v.At
		e.CaseID = v.CaseID
		e.Summary = fmt.Sprintf("case %s assigned to %s", v.CaseID, v.OfficerID)
	case *domain.CasePaused:
		e.At = v.At
		e.CaseID = v.CaseID
		e.Summary = fmt.Sprintf("case %s paused by %s: %s", v.CaseID, v.By, v.Reason)
	case *domain.CaseResumed:
		e.At = v.At
		e.CaseID = v.CaseID
		e.Summary = fmt.Sprintf("case %s resumed by %s", v.CaseID, v.By)
	case *domain.CaseReopened:
		e.At = v.At
		e.CaseID = v.CaseID
		e.Summary = fmt.Sprintf("case %s reopened epoch=%d reason=%s hits=%v", v.CaseID, v.Epoch, v.Reason, v.HitIDs)
	case *domain.CaseEscalated:
		e.At = v.At
		e.CaseID = v.CaseID
		e.Summary = fmt.Sprintf("case %s escalated (deadline %s)", v.CaseID, v.Deadline.Format(time.RFC3339))
	case *domain.ReminderFired:
		e.At = v.FiredAt
		e.CaseID = v.CaseID
		e.Summary = fmt.Sprintf("reminder %s fired for %s epoch=%d due=%s", v.Milestone, v.CaseID, v.Epoch, v.DueAt.Format(time.RFC3339))
	case *domain.NoteRecorded:
		e.At = v.Note.CreatedAt
		e.CaseID = v.Note.CaseID
		e.Summary = fmt.Sprintf("note on %s by %s", v.Note.CaseID, v.Note.AuthorID)
	case *domain.DispositionRecorded:
		e.At = v.Disposition.CreatedAt
		e.CaseID = v.Disposition.CaseID
		e.Reviewer = v.Disposition.ReviewerID
		e.Evidence = v.Disposition.EvidenceSummary
		e.Summary = fmt.Sprintf("disposition %s on %s by reviewer %s", v.Disposition.Decision, v.Disposition.CaseID, v.Disposition.ReviewerID)
		if v.Disposition.DuplicateOfCaseID != "" {
			e.Summary += " duplicate_of=" + v.Disposition.DuplicateOfCaseID
		}
	}
	return e
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func dupSuffix(id string) string {
	if id == "" {
		return ""
	}
	return " duplicate_of=" + id
}
