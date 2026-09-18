package screening

import "time"

// 事件类型
const (
	EvCustomersUpserted  = "CustomersUpserted"
	EvListEntriesImported = "ListEntriesImported"
	EvHitsIngested       = "HitsIngested"
	EvCasesAggregated    = "CasesAggregated"
	EvCasesMerged        = "CasesMerged"
	EvCaseAssigned       = "CaseAssigned"
	EvDisposition        = "DispositionRecorded"
	EvListWithdrawn      = "ListEntryWithdrawn"
	EvListRepublished    = "ListEntryRepublished"
	EvCaseSuspended      = "CaseSuspended"
	EvCaseResumed        = "CaseResumed"
	EvReminderScheduled  = "ReminderScheduled"
	EvRemindersDeferred  = "RemindersDeferred" // 暂停期间时钟顺延
	EvEscalationFired    = "EscalationFired"
	EvReminderFired      = "ReminderFired"
	EvNoteAdded          = "NoteAdded"
	EvUnmatchedHitDetached = "UnmatchedHitDetached"
)

// CaseGroup 聚合事件中的单个案件载荷。
type CaseGroup struct {
	CaseID     string   `json:"caseId"`
	HitIDs     []string `json:"hitIds"`
	CustomerID string   `json:"customerId"`
	Rules      []string `json:"rules"` // 可解释：每个案件由哪些规则聚成
}

// Event 是案件台的领域事件。持久化只追加（append-only），任何状态变更都必须先产生事件，
// 重放事件流即可在故障恢复/审计复核时重建完整状态。
type Event struct {
	Seq        int       `json:"seq"`
	OccurredAt time.Time `json:"occurredAt"` // 业务发生时刻；调度顺序以它为准，保证跨时区与故障恢复后顺序一致
	Type       string    `json:"type"`
	Actor      string    `json:"actor"`

	// 命中/聚合载荷
	Customers   []Customer  `json:"customers,omitempty"`
	ListEntries []ListEntry `json:"listEntries,omitempty"`
	Hits        []Hit       `json:"hits,omitempty"`
	// Cases 在不同事件中复用：Aggregated=本批新建案件；Merged=来源案件列表（IntoCase 为目标）
	Groups   []CaseGroup `json:"groups,omitempty"`
	Cases    []string    `json:"cases,omitempty"`
	IntoCase string      `json:"intoCase,omitempty"`

	BatchID    string `json:"batchId,omitempty"`
	CaseID     string `json:"caseId,omitempty"`
	HitID      string `json:"hitId,omitempty"` // 也用于承载提醒键
	ListID     string `json:"listId,omitempty"`
	NewVersion string `json:"newVersion,omitempty"`
	Officer    string `json:"officer,omitempty"`
	Reason     string `json:"reason,omitempty"` // 也承载处置结论
	Rule       string `json:"rule,omitempty"`
	NoteID     string `json:"noteId,omitempty"`
	Evidence   string `json:"evidence,omitempty"`
	Reviewer   string `json:"reviewer,omitempty"` // 任何解除动作必须引用复核人

	// 调度载荷
	DueAt time.Time     `json:"dueAt,omitempty"` // 截止时刻（统一存 UTC；由各地时区截止时间换算而来）
	Kind  string        `json:"kind,omitempty"`  // reminder | escalation
	Delta time.Duration `json:"delta,omitempty"` // 暂停顺延期时长

	PrevHash string `json:"prevHash,omitempty"`
	Hash     string `json:"hash,omitempty"`
}

// EventStore 只追加事件存储。
type EventStore interface {
	Append(events ...Event) (fromSeq, toSeq int, err error)
	Events() ([]Event, error)
}
