package service

import (
	"os"
	"strings"
	"testing"

	"example.com/09181/q012/internal/domain"
)

func TestAuditReplayBoundariesAndAttribution(t *testing.T) {
	svc, _, _, _ := newTestService(t, 24*time.Hour, 4*time.Hour)
	publishJohn(t, svc, "OFAC", 1)
	customer(t, svc, "C1", "John Smith", "")
	customer(t, svc, "C2", "Jon Smith", "")
	r1, err := svc.ScreenCustomers([]string{"C1", "C2"})
	must(t, err)
	must(t, svc.MergeCases("off1", r1.CaseIDs[0], r1.CaseIDs[1]))
	must(t, svc.AddNote("off1", r1.CaseIDs[0], "identity documents received"))
	must(t, svc.ResolveCase("rev1", DispositionInput{
		CaseID: r1.CaseIDs[0], Decision: domain.DecisionFalsePositive,
		EvidenceSummary: "DOB mismatch with list entry",
	}))

	rep, err := svc.AuditReplay()
	must(t, err)
	if !rep.IntegrityOK {
		t.Fatal("fresh log must verify")
	}
	if rep.LastSeq == 0 || len(rep.Events) == 0 {
		t.Fatal("audit timeline empty")
	}
	if len(rep.DispositionsWithoutAttribution) != 0 {
		t.Fatalf("all dispositions must cite reviewer and evidence: %v", rep.DispositionsWithoutAttribution)
	}

	var boundary *CaseBoundary
	for i := range rep.CaseBoundaries {
		if rep.CaseBoundaries[i].RootCaseID == r1.CaseIDs[0] {
			boundary = &rep.CaseBoundaries[i]
		}
	}
	if boundary == nil {
		t.Fatal("survivor boundary missing")
	}
	if len(boundary.AbsorbedCases) != 1 || boundary.AbsorbedCases[0] != r1.CaseIDs[1] {
		t.Fatalf("boundary must list absorbed case, got %+v", boundary)
	}
	if len(boundary.RootHitIDs) != 1 || len(boundary.AbsorbedHitIDs) != 1 {
		t.Fatalf("original hits must remain on their origin cases: %+v", boundary)
	}
	if boundary.Status != string(domain.StatusFalsePositive) {
		t.Fatalf("boundary status = %s", boundary.Status)
	}

	// 时间线里应能找到带复核人与证据的解除事件
	foundDisposition := false
	for _, e := range rep.Events {
		if e.Type == "disposition.recorded" && e.Reviewer == "rev1" &&
			strings.Contains(e.Evidence, "DOB mismatch") {
			foundDisposition = true
		}
	}
	if !foundDisposition {
		t.Fatal("audit timeline must retain reviewer and evidence on disposition")
	}
}

func TestAuditReplayDetectsTampering(t *testing.T) {
	svc, _, _, path := newTestService(t, 24*time.Hour, 4*time.Hour)
	publishJohn(t, svc, "OFAC", 1)
	customer(t, svc, "C1", "John Smith", "")
	must(t, svc.IngestHits([]HitInput{{
		CustomerID: "C1", ListID: "OFAC", EntryID: "E-JOHN", MatchedName: "John Smith", Score: 0.9,
	}}))

	raw, err := os.ReadFile(path)
	must(t, err)
	target := []byte(`"matched_name":"John Smith"`)
	if !strings.Contains(string(raw), string(target)) {
		t.Fatal("setup target missing")
	}
	raw = []byte(strings.Replace(string(raw), string(target), `"matched_name":"XXXX SmiXX"`, 1))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := svc.AuditReplay()
	if err == nil || rep.IntegrityOK {
		t.Fatalf("tampering must surface as integrity failure, got err=%v ok=%v", err, rep.IntegrityOK)
	}
}

func TestEventsCarryDeterministicOrder(t *testing.T) {
	svc, _, _, _ := newTestService(t, 24*time.Hour, 4*time.Hour)
	publishJohn(t, svc, "OFAC", 1)
	customer(t, svc, "C1", "John Smith", "")
	rep, err := svc.ScreenCustomers([]string{"C1"})
	must(t, err)

	var types []string
	_ = svc.Projection()
	// 通过审计时间线确认开案事件先于解除
	must(t, svc.ResolveCase("rev1", DispositionInput{
		CaseID: rep.OpenedCaseIDs[0], Decision: domain.DecisionTruePositive, EvidenceSummary: "confirmed",
	}))
	r2, err := svc.AuditReplay()
	must(t, err)
	for _, e := range r2.Events {
		types = append(types, e.Type)
	}
	if index(types, "case.opened") < 0 || index(types, "disposition.recorded") < 0 {
		t.Fatal("expected events missing")
	}
	if index(types, "case.opened") > index(types, "disposition.recorded") {
		t.Fatal("case must open before it can be resolved")
	}
}

func index(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}
