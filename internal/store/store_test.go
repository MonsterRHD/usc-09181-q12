package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"example.com/09181/q012/internal/domain"
)

func TestAppendAndReplayRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	es, err := NewEventStore(path)
	if err != nil {
		t.Fatal(err)
	}
	proj := NewProjection()
	if err := es.Subscribe(proj.Apply); err != nil {
		t.Fatal(err)
	}

	cust := domain.Customer{CustomerID: "C1", LegalName: "John Smith"}
	if _, err := es.Append("customer.upserted", domain.CustomerUpserted{Customer: cust, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := es.Append("list.published", domain.ListPublished{
		ListID: "L1", Version: 1,
		Entries: []domain.ListEntry{{EntryID: "E1", PrimaryName: "John Smith"}},
	}); err != nil {
		t.Fatal(err)
	}

	got, ok := proj.Customer("C1")
	if !ok || got.LegalName != "John Smith" {
		t.Fatalf("projection did not apply live events: %+v", got)
	}
	if es.Seq() != 2 {
		t.Fatalf("seq = %d, want 2", es.Seq())
	}

	// 重新打开：应校验哈希链并恢复订阅投影。
	es2, err := NewEventStore(path)
	if err != nil {
		t.Fatal(err)
	}
	proj2 := NewProjection()
	if err := es2.Subscribe(proj2.Apply); err != nil {
		t.Fatal(err)
	}
	if es2.Seq() != 2 {
		t.Fatalf("restored seq = %d, want 2", es2.Seq())
	}
	if _, ok := proj2.Customer("C1"); !ok {
		t.Fatal("restored projection missing customer")
	}
	l, ok := proj2.List("L1")
	if !ok || l.ActiveVersion() != 1 {
		t.Fatalf("restored list state wrong: %+v", l)
	}
}

func TestTamperedLogDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	es, err := NewEventStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := es.Append("customer.upserted", domain.CustomerUpserted{
		Customer: domain.Customer{CustomerID: "C1", LegalName: "A"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := es.Append("customer.upserted", domain.CustomerUpserted{
		Customer: domain.Customer{CustomerID: "C2", LegalName: "B"},
	}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 精确篡改第二条记录负载中的客户姓名（事件仅追加，任何字节修改都必须被发现）
	old := []byte(`"legal_name":"B"`)
	newB := []byte(`"legal_name":"X"`)
	if !bytesContains(raw, old) {
		t.Fatal("test setup: target bytes not found in log")
	}
	raw = replaceFirst(raw, old, newB)
	tampered := filepath.Join(t.TempDir(), "tampered.jsonl")
	if err := os.WriteFile(tampered, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEventStore(tampered); err == nil {
		t.Fatal("tampered event log must fail integrity check")
	}
}

func bytesContains(b, sub []byte) bool {
	return len(b) >= len(sub) && indexOf(b, sub) >= 0
}

func indexOf(b, sub []byte) int {
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == string(sub) {
			return i
		}
	}
	return -1
}

func replaceFirst(b, old, new []byte) []byte {
	i := indexOf(b, old)
	if i < 0 {
		return b
	}
	out := make([]byte, 0, len(b)-len(old)+len(new))
	out = append(out, b[:i]...)
	out = append(out, new...)
	out = append(out, b[i+len(old):]...)
	return out
}

func TestReminderIdempotencyKeysAcrossEpochs(t *testing.T) {
	p := NewProjection()
	anchor := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	p.Apply("case.opened", &domain.CaseOpened{
		CaseID: "CASE-1", HitIDs: []string{"HIT-1"},
		AggregationKey: "k", OpenedAt: anchor,
	})
	// 纪元 1 的预警已发
	p.Apply("reminder.fired", &domain.ReminderFired{
		CaseID: "CASE-1", Epoch: 1, Milestone: MilestoneWarning,
		DueAt: anchor.Add(20 * time.Hour), FiredAt: anchor.Add(20 * time.Hour),
	})
	due := p.DueReminders(anchor.Add(30*time.Hour), 4*time.Hour, 24*time.Hour)
	for _, d := range due {
		if d.Milestone == MilestoneWarning && d.Epoch == 1 {
			t.Fatal("fired warning must not be due again")
		}
	}
	// 案件重开进入纪元 2：旧纪元的预警不再压制新纪元
	p.Apply("case.reopened", &domain.CaseReopened{
		CaseID: "CASE-1", Epoch: 2, At: anchor.Add(48 * time.Hour),
	})
	due = p.DueReminders(anchor.Add(48*time.Hour+30*time.Hour), 4*time.Hour, 24*time.Hour)
	found := false
	for _, d := range due {
		if d.Milestone == MilestoneWarning && d.Epoch == 2 {
			found = true
		}
	}
	if !found {
		t.Fatal("new epoch must allow the warning to fire again")
	}
}

func TestDueRemindersOrderedAndSkipsPaused(t *testing.T) {
	p := NewProjection()
	anchor := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	p.Apply("case.opened", &domain.CaseOpened{CaseID: "A", AggregationKey: "ka", OpenedAt: anchor})
	p.Apply("case.opened", &domain.CaseOpened{CaseID: "B", AggregationKey: "kb", OpenedAt: anchor.Add(time.Hour)})
	p.Apply("case.paused", &domain.CasePaused{CaseID: "A", Reason: "hold", At: anchor.Add(2 * time.Hour)})

	due := p.DueReminders(anchor.Add(30*time.Hour), 4*time.Hour, 24*time.Hour)
	for _, d := range due {
		if d.CaseID == "A" {
			t.Fatal("paused case must not produce reminders")
		}
	}
	// B 已到预警（21h）与升级（25h）；A 在暂停中
	if len(due) != 2 || due[0].Milestone != MilestoneWarning || due[0].CaseID != "B" {
		t.Fatalf("unexpected due set: %+v", due)
	}
}

func TestAggregationKeyReroutedAfterMerge(t *testing.T) {
	p := NewProjection()
	anchor := time.Now()
	p.Apply("case.opened", &domain.CaseOpened{CaseID: "S", AggregationKey: "ks", OpenedAt: anchor})
	p.Apply("case.opened", &domain.CaseOpened{CaseID: "X", AggregationKey: "kx", OpenedAt: anchor})
	p.Apply("cases.merged", &domain.CasesMerged{SurvivorCaseID: "S", AbsorbedCaseID: "X"})

	id, root := p.CaseByAggregationKey("kx")
	if id != "S" || root == nil {
		t.Fatalf("absorbed key must resolve to survivor, got %q root=%v", id, root)
	}
	rootID, snap, ok := p.RootCase("X")
	if !ok || rootID != "S" || snap.CaseID != "S" {
		t.Fatalf("root resolution wrong: %q %+v %v", rootID, snap, ok)
	}
}
