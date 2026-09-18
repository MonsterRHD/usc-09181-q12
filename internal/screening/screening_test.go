package screening

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func testService() (*Service, *MemoryEventStore, MapAuthorizer) {
	store := NewMemoryEventStore()
	authz := MapAuthorizer{"officer": RoleOfficer, "reviewer": RoleReviewer, "agent": RoleAgent}
	svc := NewService(store, authz)
	return svc, store, authz
}

func baseData(t *testing.T, svc *Service) {
	t.Helper()
	if err := svc.ImportCustomers("officer", []Customer{
		{ID: "C1", LegalName: "Ivan Petrov", Aliases: []string{"I. Petrov", "Иван Петров"}, NationalID: "RUS-111", UpdatedAt: time.Now()},
		{ID: "C2", LegalName: "John Smith", Aliases: nil, NationalID: "USA-222", Address: "1 Main St", UpdatedAt: time.Now()},
		{ID: "C3", LegalName: "John Smith", NationalID: "USA-333", Address: "9 Other Ave", UpdatedAt: time.Now()},
	}); err != nil {
		t.Fatalf("import customers: %v", err)
	}
	if err := svc.ImportListEntries("officer", []ListEntry{
		{ID: "L1", Name: "Ivan Petrov", Aliases: []string{"I. Petrov"}, Program: "OFAC", Version: "v1", Active: true},
		{ID: "L2", Name: "John Smith", Program: "OFAC", Version: "v1", Active: true},
	}); err != nil {
		t.Fatalf("import list: %v", err)
	}
}

// 场景1：姓名相似、地址缺失的两个不同客户，手工最容易错并——必须被案件边界隔开。
func TestSimilarNamesDifferentCustomersNeverMerge(t *testing.T) {
	svc, _, _ := testService()
	baseData(t, svc)

	hits := []Hit{
		{ID: "H1", BatchID: "B1", CustomerID: "C2", RawName: "John Smith", Address: "", ListEntryID: "L2", ListVersion: "v1"},
		{ID: "H2", BatchID: "B1", CustomerID: "C3", RawName: "John Smith", Address: "", ListEntryID: "L2", ListVersion: "v1"},
	}
	ids, plan, err := svc.IngestBatch("officer", "B1", hits)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("期望 2 个独立案件，得到 %d (%v)", len(ids), ids)
	}
	if len(plan.KeptApart) != 1 || plan.KeptApart[0].Rule != "BOUNDARY_DIFFERENT_CUSTOMER" {
		t.Fatalf("期望记录边界隔开说明，得到 %+v", plan.KeptApart)
	}

	// 手工合并也必须被拒绝。
	err = svc.MergeCases("officer", ids[0], []string{ids[1]}, "manual")
	if !errors.Is(err, ErrCaseBoundary) {
		t.Fatalf("期望 ErrCaseBoundary，得到 %v", err)
	}
}

// 场景2：同一人多别名同时命中 -> 同一案件，规则可解释。
func TestSamePersonAliasesAggregate(t *testing.T) {
	svc, _, _ := testService()
	baseData(t, svc)

	hits := []Hit{
		{ID: "H1", BatchID: "B1", CustomerID: "C1", RawName: "Ivan Petrov", ListEntryID: "L1", ListVersion: "v1"},
		{ID: "H2", BatchID: "B1", CustomerID: "C1", RawName: "I. Petrov", ListEntryID: "L1", ListVersion: "v1"},
	}
	ids, plan, err := svc.IngestBatch("officer", "B1", hits)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("期望 1 个案件，得到 %v", ids)
	}
	found := false
	for _, e := range plan.Edges {
		if e.Rule == "CUSTOMER_ALIAS_CHAIN" {
			found = true
		}
	}
	if !found {
		t.Fatalf("期望 CUSTOMER_ALIAS_CHAIN 规则说明，得到 %+v", plan.Edges)
	}
}

// 场景3：案件合并只能追加关系，不能覆盖原命中。
func TestMergeAppendsNeverOverwrites(t *testing.T) {
	svc, store, _ := testService()
	baseData(t, svc)

	// C2 两个批次各产生一个案件（无同批连接规则，因只有一个命中每批）。
	ids1, _, err := svc.IngestBatch("officer", "B1", []Hit{
		{ID: "H1", BatchID: "B1", CustomerID: "C2", RawName: "John Smith", Address: "1 Main St", ListEntryID: "L2", ListVersion: "v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ids2, _, err := svc.IngestBatch("officer", "B2", []Hit{
		{ID: "H3", BatchID: "B2", CustomerID: "C2", RawName: "John Smith", Address: "1 Main St", NationalID: "USA-222", ListEntryID: "L2", ListVersion: "v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.MergeCases("officer", ids1[0], ids2, "MANUAL_DUPLICATE_REVIEW"); err != nil {
		t.Fatal(err)
	}

	p, err := LoadReplay(store)
	if err != nil {
		t.Fatal(err)
	}
	target := p.Cases[ids1[0]]
	if len(target.HitIDs) != 2 || !contains(target.HitIDs, "H1") || !contains(target.HitIDs, "H3") {
		t.Fatalf("目标案件应包含两条原命中，得到 %v", target.HitIDs)
	}
	if len(target.SourceCases) != 1 || target.SourceCases[0] != ids2[0] {
		t.Fatalf("应追加来源案件关系，得到 %v", target.SourceCases)
	}
	// 来源案件保留，状态为 MERGED，其材料未被删除。
	if p.Cases[ids2[0]].Status != DispMerged || len(p.Cases[ids2[0]].HitIDs) != 1 {
		t.Fatalf("来源案件应保留原命中并标记 MERGED，得到 %+v", p.Cases[ids2[0]])
	}
	if _, ok := p.Hits["H1"]; !ok {
		t.Fatal("原命中 H1 不得被覆盖删除")
	}
}

// 场景4：跨时区截止——不同时区换算到同一 UTC 时间轴，按发生顺序推动。
func TestCrossTimezoneDueOrder(t *testing.T) {
	svc, _, _ := testService()
	baseData(t, svc)
	clock := newClock(time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC))
	svc.WithClock(clock.now)

	ids, _, err := svc.IngestBatch("officer", "B1", []Hit{
		{ID: "H1", BatchID: "B1", CustomerID: "C1", RawName: "Ivan Petrov", ListEntryID: "L1", ListVersion: "v1"},
		{ID: "H2", BatchID: "B1", CustomerID: "C2", RawName: "John Smith", Address: "1 Main St", ListEntryID: "L2", ListVersion: "v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 香港 2026-09-19 09:00 HKT(+8) == 01:00 UTC；纽约 2026-09-18 21:30 EDT(-4) == 01:30 UTC。
	hkt := time.FixedZone("HKT", 8*60*60)
	nyc := time.FixedZone("EDT", -4*60*60)
	dueHK := time.Date(2026, 9, 19, 9, 0, 0, 0, hkt)
	dueNY := time.Date(2026, 9, 18, 21, 30, 0, 0, nyc)
	if err := svc.AssignCase("officer", ids[0], "officer", dueHK, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := svc.AssignCase("officer", ids[1], "officer", dueNY, time.Time{}); err != nil {
		t.Fatal(err)
	}
	fired, err := svc.PushDue(time.Date(2026, 9, 18, 2, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(fired) != 2 {
		t.Fatalf("期望推动 2 条提醒，得到 %d", len(fired))
	}
	// 01:00 UTC（香港截止）先于 01:30 UTC（纽约截止）。
	if fired[0].CaseID != ids[0] || fired[1].CaseID != ids[1] {
		t.Fatalf("期望按绝对时刻顺序 ids[0],ids[1]，得到 %s,%s", fired[0].CaseID, fired[1].CaseID)
	}
	// 再次推动不重复。
	fired2, _ := svc.PushDue(time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC))
	if len(fired2) != 0 {
		t.Fatalf("故障恢复后重复推动，得到 %d 条", len(fired2))
	}
}

// 场景5：暂停期间不推动，恢复时按暂停时长顺延；升级时钟不被消耗。
func TestSuspensionDefersReminders(t *testing.T) {
	svc, _, _ := testService()
	baseData(t, svc)
	start := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	clock := newClock(start)
	svc.WithClock(clock.now)

	ids, _, _ := svc.IngestBatch("officer", "B1", []Hit{
		{ID: "H1", BatchID: "B1", CustomerID: "C1", RawName: "Ivan Petrov", ListEntryID: "L1", ListVersion: "v1"},
	})
	reminderAt := start.Add(2 * time.Hour)
	escalationAt := start.Add(24 * time.Hour)
	if err := svc.AssignCase("officer", ids[0], "officer", reminderAt, escalationAt); err != nil {
		t.Fatal(err)
	}
	suspendAt := start.Add(30 * time.Minute)
	clock.set(suspendAt)
	if _, err := svc.SuspendCase("officer", ids[0], "等待名单方澄清"); err != nil {
		t.Fatal(err)
	}
	// 暂停期间提醒时刻已到，也不能推动。
	fired, _ := svc.PushDue(start.Add(3 * time.Hour))
	if len(fired) != 0 {
		t.Fatalf("暂停期间不应推动提醒，得到 %d", len(fired))
	}
	// 暂停 60 分钟后恢复：提醒顺延到 13:00，升级顺延到次日 11:00。
	resumeAt := suspendAt.Add(60 * time.Minute)
	clock.set(resumeAt)
	if err := svc.ResumeCase("officer", ids[0], suspendAt); err != nil {
		t.Fatal(err)
	}
	// 恢复后原截止（12:00）虽已过，但顺延后（13:00）未到，不应触发。
	fired, _ = svc.PushDue(resumeAt)
	if len(fired) != 0 {
		t.Fatalf("顺延后不应立即触发，得到 %d", len(fired))
	}
	// 顺延 60 分钟后，提醒在 13:00 触发；升级在次日 11:00 触发。
	fired, _ = svc.PushDue(resumeAt.Add(90 * time.Minute))
	if len(fired) != 1 || fired[0].Kind != "reminder" {
		t.Fatalf("期望仅 1 条提醒触发，得到 %+v", fired)
	}
	fired, _ = svc.PushDue(resumeAt.Add(24 * time.Hour))
	if len(fired) != 1 || fired[0].Kind != "escalation" {
		t.Fatalf("期望 1 条升级触发，得到 %+v", fired)
	}
}

// 场景6：升级触发后案件状态变为 ESCALATED。
func TestEscalationChangesStatus(t *testing.T) {
	svc, store, _ := testService()
	baseData(t, svc)
	start := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	clock := newClock(start)
	svc.WithClock(clock.now)
	ids, _, _ := svc.IngestBatch("officer", "B1", []Hit{
		{ID: "H1", BatchID: "B1", CustomerID: "C1", RawName: "Ivan Petrov", ListEntryID: "L1", ListVersion: "v1"},
	})
	if err := svc.AssignCase("officer", ids[0], "officer", time.Time{}, start.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PushDue(start.Add(25 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	p, _ := LoadReplay(store)
	if p.Cases[ids[0]].Status != DispEscalated {
		t.Fatalf("期望 ESCALATED，得到 %s", p.Cases[ids[0]].Status)
	}
}

// 场景7：任何解除都必须引用复核人和证据摘要。
func TestClearanceRequiresReviewerAndEvidence(t *testing.T) {
	svc, _, _ := testService()
	baseData(t, svc)
	ids, _, _ := svc.IngestBatch("officer", "B1", []Hit{
		{ID: "H1", BatchID: "B1", CustomerID: "C1", RawName: "Ivan Petrov", ListEntryID: "L1", ListVersion: "v1"},
	})
	if err := svc.RecordDisposition("officer", ids[0], DispFalsePositive, "", ""); !errors.Is(err, ErrNeedsReviewer) {
		t.Fatalf("期望 ErrNeedsReviewer，得到 %v", err)
	}
	// officer 不能冒充复核人。
	if err := svc.RecordDisposition("officer", ids[0], DispFalsePositive, "officer", "DOB 不符"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("期望 ErrForbidden，得到 %v", err)
	}
	if err := svc.RecordDisposition("officer", ids[0], DispFalsePositive, "reviewer", "出生日期与证件不符，地址不同城市"); err != nil {
		t.Fatalf("合规解除被拒: %v", err)
	}
}

// 场景8：名单回溯撤回——名单仍有效时不得解除；撤回后可凭复核解除；重新发布后再解除应被拒。
func TestListWithdrawAndRepublish(t *testing.T) {
	svc, store, _ := testService()
	baseData(t, svc)
	ids, _, _ := svc.IngestBatch("officer", "B1", []Hit{
		{ID: "H1", BatchID: "B1", CustomerID: "C1", RawName: "Ivan Petrov", ListEntryID: "L1", ListVersion: "v1"},
	})
	if err := svc.ResolveRetroactive("officer", ids[0], "reviewer", "证据"); !errors.Is(err, ErrStillActive) {
		t.Fatalf("名单仍有效，期望 ErrStillActive，得到 %v", err)
	}
	if err := svc.WithdrawListEntry("officer", "L1", "v1-withdrawn"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ResolveRetroactive("officer", ids[0], "reviewer", "发布方公告 v1 为错误条目"); err != nil {
		t.Fatalf("撤回后应可解除，得到 %v", err)
	}
	p, _ := LoadReplay(store)
	if p.Cases[ids[0]].Status != DispRetroactiveClear {
		t.Fatalf("期望 LIST_WITHDRAWN_CLEARED，得到 %s", p.Cases[ids[0]].Status)
	}
	// 撤回后重新发布（新版本）。新批次命中仍有效的新名单。
	if err := svc.RepublishListEntry("officer", ListEntry{
		ID: "L1", Name: "Ivan Petrov", Aliases: []string{"I. Petrov"}, Program: "OFAC", Version: "v2",
	}); err != nil {
		t.Fatal(err)
	}
	ids2, _, _ := svc.IngestBatch("officer", "B2", []Hit{
		{ID: "H2", BatchID: "B2", CustomerID: "C1", RawName: "Ivan Petrov", ListEntryID: "L1", ListVersion: "v2"},
	})
	if err := svc.ResolveRetroactive("officer", ids2[0], "reviewer", "证据"); !errors.Is(err, ErrStillActive) {
		t.Fatalf("新名单有效，新案件不得回溯解除，得到 %v", err)
	}
}

// 场景9：撤回与重新发布并发——两个命令基于同一版本，乐观锁保证两个事件都不丢。
func TestConcurrentWithdrawRepublish(t *testing.T) {
	svc, store, _ := testService()
	baseData(t, svc)

	// 直接构造基于同一投影版本的两个命令，模拟并发。
	p1, _ := LoadReplay(store)
	p2, _ := LoadReplay(store)
	ev1 := []Event{{Type: EvListWithdrawn, Actor: "officer", OccurredAt: time.Now().UTC(), ListID: "L1", NewVersion: "v1w"}}
	ev2 := []Event{{Type: EvListEntriesImported, Actor: "officer", OccurredAt: time.Now().UTC(),
		ListEntries: []ListEntry{{ID: "L1", Name: "Ivan Petrov", Program: "OFAC", Version: "v2", Active: true}}}}
	os1 := store
	if _, _, err := os1.AppendExpect(p1.Seq, ev1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := os1.AppendExpect(p2.Seq, ev2); !errors.Is(err, ErrConflict) {
		t.Fatalf("期望并发冲突 ErrConflict，得到 %v", err)
	}
	// Service 的重试路径在冲突后自动重放并成功。
	if err := svc.RepublishListEntry("officer", ListEntry{ID: "L1", Name: "Ivan Petrov", Program: "OFAC", Version: "v2"}); err != nil {
		t.Fatalf("重试后应成功，得到 %v", err)
	}
	p, _ := LoadReplay(store)
	if !p.ListEntries["L1"].Active || p.ListEntries["L1"].Version != "v2" {
		t.Fatalf("重发事件丢失: %+v", p.ListEntries["L1"])
	}
}

// 场景10：权限不足的客服看不到原始姓名。
func TestAgentSeesMaskedName(t *testing.T) {
	svc, store, _ := testService()
	baseData(t, svc)
	ids, _, _ := svc.IngestBatch("officer", "B1", []Hit{
		{ID: "H1", BatchID: "B1", CustomerID: "C1", RawName: "Ivan Petrov", ListEntryID: "L1", ListVersion: "v1"},
	})
	p, _ := LoadReplay(store)

	agentView := NewView(p, MapAuthorizer{"a": RoleAgent})
	cv, err := agentView.Case("a", ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if cv.Hits[0].RawName != "I*** P*****" || cv.Hits[0].RawNameVisible {
		t.Fatalf("客服应看到掩码姓名，得到 %q visible=%v", cv.Hits[0].RawName, cv.Hits[0].RawNameVisible)
	}
	officerView := NewView(p, MapAuthorizer{"o": RoleOfficer})
	cv2, _ := officerView.Case("o", ids[0])
	if cv2.Hits[0].RawName != "Ivan Petrov" || !cv2.Hits[0].RawNameVisible {
		t.Fatalf("合规官应看到原始姓名，得到 %q", cv2.Hits[0].RawName)
	}
	// 客服无权执行解除。
	if err := svc.RecordDisposition("agent", ids[0], DispFalsePositive, "reviewer", "x"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("期望 ErrForbidden，得到 %v", err)
	}
}

// 场景11：重复命中（同 ID 重新投递）不重复建案。
func TestDuplicateHitDoesNotReopen(t *testing.T) {
	svc, store, _ := testService()
	baseData(t, svc)
	h := []Hit{{ID: "H1", BatchID: "B1", CustomerID: "C1", RawName: "Ivan Petrov", ListEntryID: "L1", ListVersion: "v1"}}
	ids1, _, err := svc.IngestBatch("officer", "B1", h)
	if err != nil {
		t.Fatal(err)
	}
	ids2, _, err := svc.IngestBatch("officer", "B1-REDelivery", h)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids2) != 0 {
		t.Fatalf("重复命中不应产生新案件，得到 %v", ids2)
	}
	p, _ := LoadReplay(store)
	if len(p.Cases) != 1 || p.Cases[ids1[0]] == nil {
		t.Fatalf("应恰好 1 个案件，得到 %d", len(p.Cases))
	}
}

// 场景12：审计重放——事件哈希链检测篡改；文件存储故障恢复后状态一致。
func TestAuditReplayAndTamperDetection(t *testing.T) {
	svc, store, _ := testService()
	baseData(t, svc)
	_, _, _ = svc.IngestBatch("officer", "B1", []Hit{
		{ID: "H1", BatchID: "B1", CustomerID: "C1", RawName: "Ivan Petrov", ListEntryID: "L1", ListVersion: "v1"},
	})
	events, _ := store.Events()
	if err := VerifyChain(events); err != nil {
		t.Fatalf("完整链校验失败: %v", err)
	}
	// 篡改一条历史事件。
	tampered := make([]Event, len(events))
	copy(tampered, events)
	tampered[1].Evidence = "INJECTED"
	if err := VerifyChain(tampered); !errors.Is(err, ErrTampered) {
		t.Fatalf("期望检测到篡改，得到 %v", err)
	}
	// 删除中间事件。
	if err := VerifyChain(append(events[:1], events[2:]...)); !errors.Is(err, ErrTampered) {
		t.Fatalf("期望检测到事件缺失，得到 %v", err)
	}
}

func TestFileStoreRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	authz := MapAuthorizer{"officer": RoleOfficer, "reviewer": RoleReviewer, "agent": RoleAgent}

	store, err := OpenFileEventStore(path)
	if err != nil {
		t.Fatal(err)
	}
	svc := NewService(store, authz)
	baseData(t, svc)
	ids, _, _ := svc.IngestBatch("officer", "B1", []Hit{
		{ID: "H1", BatchID: "B1", CustomerID: "C1", RawName: "Ivan Petrov", ListEntryID: "L1", ListVersion: "v1"},
	})
	if err := svc.RecordDisposition("officer", ids[0], DispDuplicate, "reviewer", "与案件 CASE-X 为同一重复命中"); err != nil {
		t.Fatal(err)
	}

	// 模拟故障后重启：重新打开文件并重放。
	store2, err := OpenFileEventStore(path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := LoadReplay(store2)
	if err != nil {
		t.Fatalf("恢复重放失败: %v", err)
	}
	c := p.Cases[ids[0]]
	if c == nil || c.Status != DispDuplicate || c.Reviewer != "reviewer" {
		t.Fatalf("恢复后状态不一致: %+v", c)
	}
	// 恢复后继续运行：已触发语义不重复。
	svc2 := NewService(store2, authz)
	if _, err := svc2.IngestBatch("officer", "B1", []Hit{
		{ID: "H1", CustomerID: "C1", RawName: "Ivan Petrov"},
	}); err != nil {
		t.Fatal(err)
	}
	p2, _ := LoadReplay(store2)
	if len(p2.Cases) != 1 {
		t.Fatalf("恢复后重复命中不应建案，案件数 %d", len(p2.Cases))
	}
}

// 场景13：错聚材料分离——追加式，原命中与审计历史保留。
func TestDetachHitIsAppendOnly(t *testing.T) {
	svc, store, _ := testService()
	baseData(t, svc)
	// 无客户 ID 的两个同名命中凭姓名+地址聚到一起，事后发现其中一个属于他人。
	ids, _, err := svc.IngestBatch("officer", "B1", []Hit{
		{ID: "H1", BatchID: "B1", RawName: "John Smith", Address: "1 Main St", ListEntryID: "L2", ListVersion: "v1"},
		{ID: "H2", BatchID: "B1", RawName: "John Smith", Address: "1 Main St", NationalID: "USA-222", ListEntryID: "L2", ListVersion: "v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("期望初始聚合为 1 案，得到 %v", ids)
	}
	if err := svc.DetachHit("officer", ids[0], "H1", "reviewer", "证件号不匹配，H1 实际为另一客户"); err != nil {
		t.Fatal(err)
	}
	p, _ := LoadReplay(store)
	c := p.Cases[ids[0]]
	if contains(c.HitIDs, "H1") || len(c.HitIDs) != 1 {
		t.Fatalf("H1 应从活动命中中移除，得到 %v", c.HitIDs)
	}
	if _, ok := p.Hits["H1"]; !ok {
		t.Fatal("分离不得删除原命中记录")
	}
	if len(p.Detached[ids[0]]) != 1 || p.Detached[ids[0]][0] != "H1" {
		t.Fatalf("应追加分离关系，得到 %v", p.Detached)
	}
	if c.Reviewer != "reviewer" {
		t.Fatalf("分离必须引用复核人，得到 %q", c.Reviewer)
	}
}

// 工具：可控时钟。
type clock struct{ t time.Time }

func newClock(t time.Time) *clock { return &clock{t: t} }
func (c *clock) now() time.Time   { return c.t }
func (c *clock) set(t time.Time)  { c.t = t }
