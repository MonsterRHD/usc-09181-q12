package view

import (
	"strings"
	"testing"
	"time"

	"example.com/09181/q012/internal/domain"
	"example.com/09181/q012/internal/store"
)

func sampleSnapshot() store.Snapshot {
	anchor := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	hits := []domain.Hit{{
		HitID: "HIT-0001", CaseID: "CASE-0001", CustomerID: "C1",
		ListID: "OFAC", ListVersion: 1, EntryID: "E1",
		MatchedName: "John Smith", Score: 0.95,
		Reasons: []domain.MatchReason{{Rule: domain.RuleExactName, Detail: "equal to John Smith", Weight: 0.9}},
		Origin: domain.OriginLive, OccurredAt: anchor,
	}}
	return store.Snapshot{
		RootCaseID: "CASE-0001",
		Case: store.CaseState{
			CaseID: "CASE-0001", CustomerID: "C1", Status: domain.StatusOpen,
			AggregationRule: "same_customer_same_list", Timezone: "Asia/Singapore",
			HitIDs: []string{"HIT-0001"}, OpenedAt: anchor, AnchorAt: anchor, Epoch: 1,
		},
		Hits: hits,
		Notes: []domain.Note{{
			CaseID: "CASE-0001", AuthorID: "off1", Content: "passport copy on file", CreatedAt: anchor,
		}},
		Disposition: &domain.Disposition{
			CaseID: "CASE-0001", ReviewerID: "rev1", Decision: domain.DecisionFalsePositive,
			EvidenceSummary: "DOB mismatch", CreatedAt: anchor,
		},
	}
}

func customerPtr() *domain.Customer {
	return &domain.Customer{
		CustomerID: "C1", LegalName: "John Smith", Aliases: []string{"Johnny"},
	}
}

func TestAgentViewRedactsSensitiveFields(t *testing.T) {
	v := ForRole(domain.RoleAgent).Case(sampleSnapshot(), customerPtr())

	if v.Hits[0].MatchedName != RedactedPlaceholder || !v.Hits[0].Redacted {
		t.Fatalf("agent must not see original matched name, got %q", v.Hits[0].MatchedName)
	}
	if v.Hits[0].Reasons[0].Detail != "" {
		t.Fatal("reason detail (embeds list name) must be cleared for agent")
	}
	if v.Hits[0].Reasons[0].Rule != domain.RuleExactName {
		t.Fatal("rule code itself may stay visible")
	}
	if len(v.Notes) != 0 {
		t.Fatalf("agent must not see investigation notes, got %+v", v.Notes)
	}
	if v.Disposition == nil || v.Disposition.EvidenceSummary != RedactedPlaceholder {
		t.Fatalf("evidence must be redacted for agent, got %+v", v.Disposition)
	}
	if v.Customer == nil || v.Customer.LegalName != RedactedPlaceholder {
		t.Fatalf("customer legal name must be redacted, got %+v", v.Customer)
	}
	if strings.Contains(jsonFlat(v), "John Smith") || strings.Contains(jsonFlat(v), "passport copy") {
		t.Fatal("redacted view must not leak original names or note content")
	}
}

func TestOfficerViewShowsSensitiveFields(t *testing.T) {
	v := ForRole(domain.RoleOfficer).Case(sampleSnapshot(), customerPtr())
	if v.Hits[0].MatchedName != "John Smith" || v.Hits[0].Redacted {
		t.Fatal("officer must see the original name")
	}
	if len(v.Notes) != 1 || v.Notes[0].Content != "passport copy on file" {
		t.Fatal("officer must see investigation notes")
	}
	if v.Customer.LegalName != "John Smith" {
		t.Fatal("officer must see customer legal name")
	}
	if v.Disposition.EvidenceSummary != "DOB mismatch" {
		t.Fatal("officer may see evidence summary")
	}
}

func TestOriginCaseSurvivesMergeInView(t *testing.T) {
	snap := sampleSnapshot()
	snap.RelatedCases = []string{"CASE-0001", "CASE-0002"}
	v := ForRole(domain.RoleOfficer).Case(snap, nil)
	if v.Hits[0].OriginCase != "CASE-0001" || v.Hits[0].CaseID != "CASE-0001" {
		t.Fatalf("origin vs current case attribution must both be visible: %+v", v.Hits[0])
	}
}

// jsonFlat 拼接全部敏感字段，用于泄漏检查。
func jsonFlat(v CaseView) string {
	var sb strings.Builder
	sb.WriteString(v.CaseID)
	for _, h := range v.Hits {
		sb.WriteString(h.MatchedName)
		for _, r := range h.Reasons {
			sb.WriteString(r.Detail)
		}
	}
	for _, n := range v.Notes {
		sb.WriteString(n.Content)
	}
	if v.Disposition != nil {
		sb.WriteString(v.Disposition.EvidenceSummary)
	}
	if v.Customer != nil {
		sb.WriteString(v.Customer.LegalName)
		for _, a := range v.Customer.Aliases {
			sb.WriteString(a)
		}
	}
	return sb.String()
}
