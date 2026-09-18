package service

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"example.com/09181/q012/internal/domain"
	"example.com/09181/q012/internal/store"
)

// ---------------------------------------------------------------------------
// 测试夹具
// ---------------------------------------------------------------------------

type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fixedClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type recordSink struct {
	mu     sync.Mutex
	got    []Reminder
	failN  int
	calls  int
}

func (s *recordSink) Deliver(_ context.Context, r Reminder) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.failN > 0 {
		s.failN--
		return os.ErrDeadlineExceeded
	}
	s.got = append(s.got, r)
	return nil
}

func (s *recordSink) delivered() []Reminder {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Reminder, len(s.got))
	copy(out, s.got)
	return out
}

func newTestService(t *testing.T, sla, warn time.Duration) (*Service, *fixedClock, *recordSink, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	es, err := store.NewEventStore(path)
	if err != nil {
		t.Fatalf("event store: %v", err)
	}
	clk := &fixedClock{t: time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)}
	sink := &recordSink{}
	svc, err := New(es, Config{SLA: sla, WarningLead: warn, Clock: clk.now, Sink: sink})
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	seed(t, svc)
	return svc, clk, sink, path
}

func seed(t *testing.T, svc *Service) {
	t.Helper()
	must(t, svc.RegisterOfficer(domain.Officer{OfficerID: "agent1", Name: "A Agent", Role: domain.RoleAgent}))
	must(t, svc.RegisterOfficer(domain.Officer{OfficerID: "off1", Name: "O Officer", Role: domain.RoleOfficer}))
	must(t, svc.RegisterOfficer(domain.Officer{OfficerID: "rev1", Name: "R Reviewer", Role: domain.RoleReviewer}))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func wantErrIs(t *testing.T, err error, target error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error %v, got nil", target)
	}
	if !errorIs(err, target) {
		t.Fatalf("expected error %v, got %v", target, err)
	}
}

func errorIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func entryJohn() []domain.ListEntry {
	return []domain.ListEntry{{EntryID: "E-JOHN", PrimaryName: "John Smith", DateOfBirth: "1980-01-01"}}
}

func publishJohn(t *testing.T, svc *Service, listID string, version int) {
	t.Helper()
	must(t, svc.PublishList(listID, version, entryJohn(), time.Time{}))
}

func customer(t *testing.T, svc *Service, id, name, tz string, aliases ...string) {
	t.Helper()
	must(t, svc.UpsertCustomer(domain.Customer{
		CustomerID: id, LegalName: name, Aliases: aliases, Timezone: tz,
	}))
}

// ---------------------------------------------------------------------------
// 案件聚合边界
// ---------------------------------------------------------------------------

func TestAggregationKeepsCustomersApart(t *testing.T) {
	svc, _, _, _ := newTestService(t, 24*time.Hour, 4*time.Hour)
	publishJohn(t, svc, "OFAC", 1)
	// 两个不同客户，姓名相似且都没有地址：不得并案。
	customer(t, svc, "C1", "John Smith", "")
	customer(t, svc, "C2", "Jon Smith", "")

	rep, err := svc.ScreenCustomers([]string{"C1", "C2"})
	must(t, err)
	if len(rep.OpenedCaseIDs) != 2 {
		t.Fatalf("expected 2 cases for 2 customers, got %+v", rep)
	}
	if rep.CaseIDs[0] == rep.CaseIDs[1] {
		t.Fatalf("two customers must not share a case: %v", rep.CaseIDs)
	}

	// 同一客户的多个别名同时命中，仍归入同一案件。
	must(t, svc.UpsertCustomer(domain.Customer{
		CustomerID: "C3", LegalName: "John Smith", Aliases: []string{"J. Smith", "Johnny Smith"},
	}))
	if _, err := svc.ScreenCustomers([]string{"C3"}); err != nil {
		t.Fatal(err)
	}
	if cases3 := rootCaseIDs(svc, "C3"); len(cases3) != 1 {
		t.Fatalf("all aliases of one customer must aggregate to one case, got %v", cases3)
	}
}

func rootCaseIDs(svc *Service, customerID string) []string {
	var out []string
	for _, c := range svc.Projection().Cases() {
		if c.CustomerID == customerID {
			out = append(out, c.CaseID)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 合并只能追加关系，不能覆盖原命中
// ---------------------------------------------------------------------------

func TestMergeAppendsRelationAndPreservesOriginalHits(t *testing.T) {
	svc, _, _, _ := newTestService(t, 24*time.Hour, 4*time.Hour)
	publishJohn(t, svc, "OFAC", 1)
	customer(t, svc, "C1", "John Smith", "")
	customer(t, svc, "C2", "Jon Smith", "")
	_, err := svc.ScreenCustomers([]string{"C1", "C2"})
	must(t, err)

	cs := svc.Projection().Cases()
	if len(cs) != 2 {
		t.Fatalf("setup: want 2 cases, got %d", len(cs))
	}
	survivor, absorbed := cs[0].CaseID, cs[1].CaseID

	must(t, svc.MergeCases("off1", survivor, absorbed))

	// 被吸收案件不能再直接操作
	if err := svc.AddNote("off1", absorbed, "x"); !errorIs(err, domain.ErrCaseAbsorbed) {
		t.Fatalf("acting on absorbed case must fail, got %v", err)
	}
	// 重复合并被拒绝
	wantErrIs(t, svc.MergeCases("off1", survivor, absorbed), domain.ErrCasesAlreadyRelated)

	snap, ok := svc.Projection().CaseSnapshot(survivor)
	if !ok {
		t.Fatal("snapshot missing")
	}
	if len(snap.Hits) != 2 {
		t.Fatalf("merged view must show both hits, got %d", len(snap.Hits))
	}
	// 原始命中仍挂在各自最初案件上（未被搬移）
	originCases := map[string]string{}
	for _, h := range snap.Hits {
		originCases[h.HitID] = h.CaseID
	}
	if originCases[snap.Hits[0].HitID] != survivor {
		t.Error("survivor hit moved")
	}
	foundAbsorbed := false
	for _, h := range snap.Hits {
		if h.CaseID == absorbed {
			foundAbsorbed = true
		}
	}
	if !foundAbsorbed {
		t.Error("absorbed case's original hit must remain attributed to it")
	}

	// 被吸收案件的新同键命中应通过 caseByKey 解析到幸存案件
	rep, err := svc.IngestHits([]HitInput{{
		CustomerID: "C2", ListID: "OFAC", EntryID: "E-JOHN",
		MatchedName: "Jon Smith", Score: 0.9,
	}})
	must(t, err)
	if rep.CaseIDs[0] != survivor {
		t.Fatalf("new hit for absorbed customer must land on survivor, got %s", rep.CaseIDs[0])
	}
}

// ---------------------------------------------------------------------------
// 暂停冻结 SLA，提醒按到期顺序且幂等
// ---------------------------------------------------------------------------

func TestPauseFreezesSLA(t *testing.T) {
	svc, clk, sink, _ := newTestService(t, 24*time.Hour, 4*time.Hour)
	publishJohn(t, svc, "OFAC", 1)
	customer(t, svc, "C1", "John Smith", "")
	rep, err := svc.ScreenCustomers([]string{"C1"})
	must(t, err)
	caseID := rep.OpenedCaseIDs[0]

	// t0+10h 暂停，持续 10 小时：预警/升级整体顺延 10h
	clk.add(10 * time.Hour)
	must(t, svc.PauseCase("off1", caseID, "awaiting documents"))
	clk.add(10 * time.Hour) // t=20h（原本预警此刻到期）
	must(t, svc.ResumeCase("off1", caseID))

	fired, err := svc.PumpReminders(context.Background())
	must(t, err)
	if len(fired) != 0 {
		t.Fatalf("paused interval must defer the warning (now 20h, due 30h), got %+v", fired)
	}

	clk.add(10 * time.Hour) // t=30h：顺延后的预警到期
	fired, err = svc.PumpReminders(context.Background())
	must(t, err)
	if len(fired) != 1 || fired[0].Milestone != "warning" {
		t.Fatalf("expected one warning after pause-adjusted deadline, got %+v", fired)
	}

	clk.add(2 * time.Hour) // t=32h：升级原定顺延后为 34h，不应有新提醒
	fired, _ = svc.PumpReminders(context.Background())
	if len(fired) != 0 {
		t.Fatalf("reminder must be idempotent, got %+v", fired)
	}
	clk.add(2 * time.Hour) // t=34h：升级到期
	fired, _ = svc.PumpReminders(context.Background())
	if len(fired) != 1 || fired[0].Milestone != "escalation" {
		t.Fatalf("expected escalation, got %+v", fired)
	}
	snap, _ := svc.Projection().CaseSnapshot(caseID)
	if snap.Case.Status != domain.StatusEscalated {
		t.Fatalf("case should be escalated, got %s", snap.Case.Status)
	}
	if len(sink.delivered()) != 2 {
		t.Fatalf("sink should have exactly two deliveries, got %d", len(sink.delivered()))
	}
}

func TestRemindersOrderedAndRecoveredAfterRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	clk := &fixedClock{t: time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)}
	sink := &recordSink{}

	boot := func() *Service {
		es, err := store.NewEventStore(path)
		must(t, err)
		svc, err := New(es, Config{SLA: 24 * time.Hour, WarningLead: 4 * time.Hour, Clock: clk.now, Sink: sink})
		must(t, err)
		return svc
	}
	svc := boot()
	seed(t, svc)
	publishJohn(t, svc, "OFAC", 1)
	customer(t, svc, "C1", "John Smith", "")
	customer(t, svc, "C2", "Jon Smith", "")
	must(t, svc.IngestHits([]HitInput{
		{CustomerID: "C1", ListID: "OFAC", EntryID: "E-JOHN", MatchedName: "John Smith", Score: 0.9, OccurredAt: clk.t},
	}))
	// 第二个案件晚开 1 小时
	clk.add(1 * time.Hour)
	must(t, svc.IngestHits([]HitInput{
		{CustomerID: "C2", ListID: "OFAC", EntryID: "E-JOHN", MatchedName: "Jon Smith", Score: 0.9, OccurredAt: clk.t},
	}))

	// 到达第一个案件的预警时刻：仅一个，且是先开的案件
	clk.add(19 * time.Hour) // 距 t0 20h
	fired, err := svc.PumpReminders(context.Background())
	must(t, err)
	if len(fired) != 1 || fired[0].CaseID == "" {
		t.Fatalf("expected the earlier case only, got %+v", fired)
	}
	firstCase := fired[0].CaseID

	// 模拟故障恢复：重启进程，已发提醒不得补发
	svc2 := boot()
	fired, err = svc2.PumpReminders(context.Background())
	must(t, err)
	if len(fired) != 0 {
		t.Fatalf("recovered process must not resend fired reminders, got %+v", fired)
	}

	// 推进到第二个案件预警到期（其锚点晚 1h）：按到期顺序补发且不重发
	clk.add(1 * time.Hour)
	fired, _ = svc2.PumpReminders(context.Background())
	if len(fired) != 1 {
		t.Fatalf("expected second warning in due order, got %+v", fired)
	}
	if fired[0].CaseID == firstCase {
		t.Fatal("reminders must follow due order across restart")
	}
}

func TestReminderDeliveryRetryAfterFailure(t *testing.T) {
	svc, clk, sink, _ := newTestService(t, 24*time.Hour, 4*time.Hour)
	sink.failN = 1
	publishJohn(t, svc, "OFAC", 1)
	customer(t, svc, "C1", "John Smith", "")
	_, err := svc.ScreenCustomers([]string{"C1"})
	must(t, err)
	clk.add(20 * time.Hour)

	fired, _ := svc.PumpReminders(context.Background())
	if len(fired) != 1 || len(sink.delivered()) != 0 {
		t.Fatalf("failed delivery still records the fired reminder, delivered=%d", len(sink.delivered()))
	}
	// 下次泵先补发积压
	fired, _ = svc.PumpReminders(context.Background())
	if len(fired) != 1 || fired[0].Milestone != "warning" || len(sink.delivered()) != 1 {
		t.Fatalf("undelivered reminder should be retried first, fired=%+v delivered=%d", fired, len(sink.delivered()))
	}
}

func TestTimezoneDeadlineRenderedLocally(t *testing.T) {
	svc, _, _, _ := newTestService(t, 24*time.Hour, 4*time.Hour)
	publishJohn(t, svc, "OFAC", 1)
	customer(t, svc, "C1", "John Smith", "Asia/Singapore")
	rep, err := svc.ScreenCustomers([]string{"C1"})
	must(t, err)
	snap, _ := svc.Projection().CaseSnapshot(rep.OpenedCaseIDs[0])
	d := svc.CaseDeadline(snap)
	if d.LocalEscalation.Location().String() != "Asia/Singapore" {
		t.Fatalf("expected Singapore zone, got %s", d.LocalEscalation.Location())
	}
	// UTC 09:00 + 24h → SGT 17:00 次日 (UTC+8)
	if d.LocalEscalation.Hour() != 17 {
		t.Fatalf("local escalation hour want 17, got %d", d.LocalEscalation.Hour())
	}
	if !d.EscalationAt.Equal(d.LocalEscalation.UTC()) {
		t.Fatal("absolute and local deadlines must describe the same instant")
	}
}

// ---------------------------------------------------------------------------
// 解除守卫与回溯重开
// ---------------------------------------------------------------------------

func TestDispositionRequiresReviewerAndEvidence(t *testing.T) {
	svc, _, _, _ := newTestService(t, 24*time.Hour, 4*time.Hour)
	publishJohn(t, svc, "OFAC", 1)
	customer(t, svc, "C1", "John Smith", "")
	rep, _ := svc.ScreenCustomers([]string{"C1"})
	caseID := rep.OpenedCaseIDs[0]

	// 客服与合规官都不能解除
	wantErrIs(t, svc.ResolveCase("off1", DispositionInput{
		CaseID: caseID, Decision: domain.DecisionFalsePositive, EvidenceSummary: "x",
	}), domain.ErrRoleNotPermitted)

	// 证据摘要必填
	wantErrIs(t, svc.ResolveCase("rev1", DispositionInput{
		CaseID: caseID, Decision: domain.DecisionFalsePositive,
	}), domain.ErrEvidenceRequired)

	// 重复命中必须引用另一案件
	wantErrIs(t, svc.ResolveCase("rev1", DispositionInput{
		CaseID: caseID, Decision: domain.DecisionDuplicate, EvidenceSummary: "dup",
	}), domain.ErrInvalidInput)

	// 合法解除
	must(t, svc.ResolveCase("rev1", DispositionInput{
		CaseID: caseID, Decision: domain.DecisionFalsePositive, EvidenceSummary: "DOB and nationality differ; documents checked",
	}))
	snap, _ := svc.Projection().CaseSnapshot(caseID)
	if snap.Case.Status != domain.StatusFalsePositive || snap.Disposition.ReviewerID != "rev1" {
		t.Fatalf("disposition not applied: %+v", snap.Case)
	}
	// 终态案件不能再次解除
	wantErrIs(t, svc.ResolveCase("rev1", DispositionInput{
		CaseID: caseID, Decision: domain.DecisionTruePositive, EvidenceSummary: "x",
	}), domain.ErrCaseTerminal)
}

func TestRetroactiveRescanReopensFalsePositiveOnly(t *testing.T) {
	svc, _, _, _ := newTestService(t, 24*time.Hour, 4*time.Hour)
	publishJohn(t, svc, "OFAC", 1)
	customer(t, svc, "C1", "John Smith", "")
	rep, _ := svc.ScreenCustomers([]string{"C1"})
	caseID := rep.OpenedCaseIDs[0]
	must(t, svc.ResolveCase("rev1", DispositionInput{
		CaseID: caseID, Decision: domain.DecisionFalsePositive, EvidenceSummary: "weak name only",
	}))

	// 撤回后才能重新发布
	must(t, svc.WithdrawList("OFAC", 1))
	wantErrIs(t, svc.RepublishList("OFAC", 1, 2, entryJohn(), time.Time{}), domain.ErrListNotWithdrawn)

	// v2 重新发布 → 回溯命中，误报案件重开并进入新提醒纪元
	r2, err := svc.RepublishList("OFAC", 1, 2, entryJohn(), time.Time{})
	must(t, err)
	if len(r2.ReopenedCaseIDs) != 1 || r2.ReopenedCaseIDs[0] != caseID {
		t.Fatalf("false-positive case should reopen, got %+v", r2)
	}
	snap, _ := svc.Projection().CaseSnapshot(caseID)
	if snap.Case.Status != domain.StatusOpen || snap.Case.Epoch != 2 {
		t.Fatalf("case should be reopened epoch 2, got status=%s epoch=%d", snap.Case.Status, snap.Case.Epoch)
	}
	if len(snap.Hits) != 2 {
		t.Fatalf("retroactive hit should be appended, got %d", len(snap.Hits))
	}

	// 真阳性案件不随回溯重开
	customer(t, svc, "C2", "Jon Smith", "")
	live, _ := svc.ScreenCustomers([]string{"C2"})
	c2 := live.OpenedCaseIDs[0]
	must(t, svc.ResolveCase("rev1", DispositionInput{
		CaseID: c2, Decision: domain.DecisionTruePositive, EvidenceSummary: "confirmed identity with documents",
	}))
	must(t, svc.WithdrawList("OFAC", 2))
	r3, err := svc.RepublishList("OFAC", 2, 3, entryJohn(), time.Time{})
	must(t, err)
	for _, id := range r3.ReopenedCaseIDs {
		if id == c2 {
			t.Fatal("true-positive case must not reopen on retroactive rescan")
		}
	}
	snap2, _ := svc.Projection().CaseSnapshot(c2)
	if snap2.Case.Status != domain.StatusTruePositive {
		t.Fatalf("true-positive must remain resolved, got %s", snap2.Case.Status)
	}
}

func TestDuplicateHitsAreMarkedNotAggregatedAway(t *testing.T) {
	svc, _, _, _ := newTestService(t, 24*time.Hour, 4*time.Hour)
	publishJohn(t, svc, "OFAC", 1)
	customer(t, svc, "C1", "John Smith", "")

	first, err := svc.ScreenCustomers([]string{"C1"})
	must(t, err)
	second, err := svc.ScreenCustomers([]string{"C1"})
	must(t, err)
	if second.Duplicates != 1 {
		t.Fatalf("second identical screening should mark 1 duplicate, got %d", second.Duplicates)
	}
	if second.CaseIDs[0] != first.CaseIDs[0] {
		t.Fatal("duplicate hits belong to the same case")
	}
	h, ok := svc.Projection().Hit(second.HitIDs[0])
	if !ok || h.DuplicateOf != first.HitIDs[0] {
		t.Fatalf("duplicate link missing: %+v ok=%v", h, ok)
	}
	if h.CaseID != first.CaseIDs[0] {
		t.Fatal("original hit ownership must point at the original case")
	}
}

// ---------------------------------------------------------------------------
// 并发：同一客户的并发批次只开出一个案件
// ---------------------------------------------------------------------------

func TestConcurrentBatchesOpenSingleCase(t *testing.T) {
	svc, _, _, _ := newTestService(t, 24*time.Hour, 4*time.Hour)
	publishJohn(t, svc, "OFAC", 1)
	customer(t, svc, "C1", "John Smith", "")

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.IngestHits([]HitInput{{
				CustomerID: "C1", ListID: "OFAC", EntryID: "E-JOHN",
				MatchedName: "John Smith", Score: 0.9,
			}})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must(t, err)
	}
	if n := len(svc.Projection().Cases()); n != 1 {
		t.Fatalf("concurrent batches for one customer must open exactly 1 case, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// 暂停动作守卫
// ---------------------------------------------------------------------------

func TestPauseResumeGuards(t *testing.T) {
	svc, _, _, _ := newTestService(t, 24*time.Hour, 4*time.Hour)
	publishJohn(t, svc, "OFAC", 1)
	customer(t, svc, "C1", "John Smith", "")
	rep, _ := svc.ScreenCustomers([]string{"C1"})
	caseID := rep.OpenedCaseIDs[0]

	wantErrIs(t, svc.PauseCase("agent1", caseID, "x"), domain.ErrRoleNotPermitted)
	wantErrIs(t, svc.PauseCase("off1", caseID, " "), domain.ErrInvalidInput)
	must(t, svc.PauseCase("off1", caseID, "waiting"))
	wantErrIs(t, svc.PauseCase("off1", caseID, "again"), domain.ErrCasePaused)
	// 暂停期间不能解除或合并案件
	wantErrIs(t, svc.ResolveCase("rev1", DispositionInput{
		CaseID: caseID, Decision: domain.DecisionFalsePositive, EvidenceSummary: "x",
	}), domain.ErrCasePaused)
	must(t, svc.ResumeCase("off1", caseID))
	wantErrIs(t, svc.ResumeCase("off1", caseID), domain.ErrCaseNotPaused)
}

// ---------------------------------------------------------------------------
// 无客户号的游离命中绝不互相聚合
// ---------------------------------------------------------------------------

func TestUnidentifiedHitsNeverMerge(t *testing.T) {
	svc, _, _, _ := newTestService(t, 24*time.Hour, 4*time.Hour)
	publishJohn(t, svc, "OFAC", 1)
	rep, err := svc.IngestHits([]HitInput{
		{ListID: "OFAC", EntryID: "E-JOHN", MatchedName: "John Smith", Score: 0.9},
		{ListID: "OFAC", EntryID: "E-JOHN", MatchedName: "John Smith", Score: 0.9},
	})
	must(t, err)
	if rep.CaseIDs[0] == rep.CaseIDs[1] {
		t.Fatal("hits without customer identity must never share a case")
	}
}
