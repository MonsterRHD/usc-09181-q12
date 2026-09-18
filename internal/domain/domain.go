// Package domain 持有制裁筛查案件台的核心领域模型、状态机定义、
// 不可变事件以及与传输方式无关的错误。
//
// 设计约束（见 README「业务不变量」）：
//   - 原始命中不可变：案件聚合与合并只能追加关联，不能覆盖或搬移原命中；
//   - 解除（处置）必须引用复核人与证据摘要；
//   - 名单按版本发布/撤回/重新发布，重新发布触发回溯；
//   - 误报、真阳性、重复命中、回溯、升级时限均为显式状态。
package domain

import (
	"errors"
	"time"
)

// ---------------------------------------------------------------------------
// 名单与客户主数据
// ---------------------------------------------------------------------------

// ListStatus 名单某一版本的状态。
type ListStatus string

const (
	ListActive    ListStatus = "active"    // 现行有效版本
	ListWithdrawn ListStatus = "withdrawn" // 已撤回，不能再产生实时命中
)

// ListEntry 名单中的一个条目（被制裁对象）。
type ListEntry struct {
	EntryID     string   `json:"entry_id"`
	PrimaryName string   `json:"primary_name"`
	Aliases     []string `json:"aliases,omitempty"`
	DateOfBirth string   `json:"date_of_birth,omitempty"` // ISO yyyy-mm-dd，可空
	Nationality string   `json:"nationality,omitempty"`
	IDDocuments []string `json:"id_documents,omitempty"`
	Program     string   `json:"program,omitempty"`
}

// Address 客户地址；跨境客户常常缺失或不完整，缺失地址不得作为同一性依据。
type Address struct {
	Line       string `json:"line,omitempty"`
	City       string `json:"city,omitempty"`
	Region     string `json:"region,omitempty"`
	PostalCode string `json:"postal_code,omitempty"`
	Country    string `json:"country,omitempty"`
}

// Customer 客户主数据。同一自然人的多个别名归集在同一条主数据下。
type Customer struct {
	CustomerID  string    `json:"customer_id"`
	LegalName   string    `json:"legal_name"`
	Aliases     []string  `json:"aliases,omitempty"`
	DateOfBirth string    `json:"date_of_birth,omitempty"`
	Nationality string    `json:"nationality,omitempty"`
	IDDocuments []string  `json:"id_documents,omitempty"`
	Addresses   []Address `json:"addresses,omitempty"`
	// Timezone 为 IANA 时区名，跨时区截止时间按此时区解释为绝对时刻。
	Timezone string `json:"timezone,omitempty"`
}

// MatchReason 解释一条命中为什么产生。Weight 为对总分的贡献，0 表示仅作标注。
type MatchReason struct {
	Rule   string  `json:"rule"`
	Detail string  `json:"detail,omitempty"`
	Weight float64 `json:"weight"`
}

const (
	RuleExactName       = "exact_name"          // 与主名/别名完全一致
	RuleFuzzyName       = "fuzzy_name"          // 模糊姓名达到阈值
	RuleDOB             = "date_of_birth"       // 出生日期一致（加分）
	RuleNationality     = "nationality"         // 国籍一致（加分）
	RuleIDDocument      = "id_document"         // 证件号一致（强佐证）
	RuleWeakDemographic = "weak_demographics"   // 仅姓名相似、无强佐证
	RuleAddressMissing  = "address_missing"     // 客户无地址，证据偏弱
	RuleDOBVeto         = "date_of_birth_veto"  // 出生日期冲突，判定非同一人
)

// HitOrigin 命中来源：实时筛查或名单重发后的回溯重扫。
type HitOrigin string

const (
	OriginLive        HitOrigin = "live"
	OriginRetroactive HitOrigin = "retroactive"
)

// Hit 一条不可变的筛查命中。CaseID 记录它最初落入的案件（原始归属），
// 案件合并后该字段仍指向原案件，以此保证「原命中不被覆盖」。
type Hit struct {
	HitID        string        `json:"hit_id"`
	CaseID       string        `json:"case_id"`
	CustomerID   string        `json:"customer_id,omitempty"`
	ListID       string        `json:"list_id"`
	ListVersion  int           `json:"list_version"`
	EntryID      string        `json:"entry_id"`
	MatchedName  string        `json:"matched_name"` // 客户侧参与匹配的原始姓名
	Score        float64       `json:"score"`
	Reasons      []MatchReason `json:"reasons"`
	Origin       HitOrigin     `json:"origin"`
	DuplicateOf  string        `json:"duplicate_of,omitempty"` // 业务重复命中指向原命中
	OccurredAt   time.Time     `json:"occurred_at"`
}

// BusinessKey 用于识别「重复命中」：同一客户、同一名单版本、同一条目、同一姓名。
func (h Hit) BusinessKey() string {
	return h.CustomerID + "|" + h.ListID + "|" + itoa(h.ListVersion) + "|" + h.EntryID + "|" + h.MatchedName
}

// ---------------------------------------------------------------------------
// 案件状态机
// ---------------------------------------------------------------------------

// CaseStatus 案件状态。误报/真阳性/重复命中是终态处置，合并后案件为 merged_away。
type CaseStatus string

const (
	StatusOpen          CaseStatus = "open"
	StatusPaused        CaseStatus = "paused"
	StatusEscalated     CaseStatus = "escalated" // 超过升级时限仍未解除
	StatusFalsePositive CaseStatus = "resolved_false_positive"
	StatusTruePositive  CaseStatus = "resolved_true_positive"
	StatusDuplicate     CaseStatus = "resolved_duplicate"
	StatusMergedAway    CaseStatus = "merged_away" // 已通过追加关系并入其他案件
)

// Terminal 表示案件已解除或已并入他案，不得再推进。
func (s CaseStatus) Terminal() bool {
	switch s {
	case StatusFalsePositive, StatusTruePositive, StatusDuplicate, StatusMergedAway:
		return true
	}
	return false
}

// Decision 复核决定，对应三类显式终态。
type Decision string

const (
	DecisionFalsePositive Decision = "false_positive"
	DecisionTruePositive  Decision = "true_positive"
	DecisionDuplicate     Decision = "duplicate"
)

// Valid 校验复核决定取值。
func (d Decision) Valid() bool {
	switch d {
	case DecisionFalsePositive, DecisionTruePositive, DecisionDuplicate:
		return true
	}
	return false
}

// Status 把复核决定映射为案件终态。
func (d Decision) Status() CaseStatus {
	switch d {
	case DecisionFalsePositive:
		return StatusFalsePositive
	case DecisionTruePositive:
		return StatusTruePositive
	default:
		return StatusDuplicate
	}
}

// Note 调查笔记，只增不改（审计要求）。
type Note struct {
	CaseID    string    `json:"case_id"`
	AuthorID  string    `json:"author_id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// Disposition 处置/解除记录。任何解除都必须引用复核人与证据摘要。
type Disposition struct {
	CaseID            string    `json:"case_id"`
	ReviewerID        string    `json:"reviewer_id"`
	Decision          Decision  `json:"decision"`
	EvidenceSummary   string    `json:"evidence_summary"`
	DuplicateOfCaseID string    `json:"duplicate_of_case_id,omitempty"` // decision=duplicate 时必填
	CreatedAt         time.Time `json:"created_at"`
}

// ---------------------------------------------------------------------------
// 事件（仅追加，审计重放的唯一事实来源）
// ---------------------------------------------------------------------------

type CustomerUpserted struct {
	Customer Customer  `json:"customer"`
	At       time.Time `json:"at"`
}

// Role 为人员在案件台上的角色，决定可见字段与可执行动作。
type Role string

const (
	RoleAgent   Role = "agent"   // 客服：看不到原始姓名等敏感字段
	RoleOfficer Role = "officer" // 合规官：承办、合并、暂停、记笔记
	RoleReviewer Role = "reviewer" // 复核人：可作出解除决定
)

// Officer 人员名册条目。
type Officer struct {
	OfficerID string `json:"officer_id"`
	Name      string `json:"name"`
	Role      Role   `json:"role"`
	// Disabled 后不再是有效操作主体（历史审计记录仍保留其姓名）。
	Disabled bool `json:"disabled,omitempty"`
}

type OfficerProvisioned struct {
	Officer Officer   `json:"officer"`
	At      time.Time `json:"at"`
}

type ListPublished struct {
	ListID      string      `json:"list_id"`
	Version     int         `json:"version"`
	Entries     []ListEntry `json:"entries"`
	PublishedAt time.Time   `json:"published_at"`
}

type ListWithdrawn struct {
	ListID      string    `json:"list_id"`
	Version     int       `json:"version"`
	WithdrawnAt time.Time `json:"withdrawn_at"`
}

// ListRepublished 撤回后的名单重新发布为新版本，并触发对存量客户的回溯重扫。
type ListRepublished struct {
	ListID      string      `json:"list_id"`
	Version     int         `json:"version"`
	BaseVersion int         `json:"base_version"`
	Entries     []ListEntry `json:"entries"`
	At          time.Time   `json:"at"`
}

type HitRecorded struct {
	Hit Hit `json:"hit"`
}

// CaseOpened 按可解释的聚合规则开立案件。
type CaseOpened struct {
	CaseID          string    `json:"case_id"`
	CustomerID      string    `json:"customer_id,omitempty"`
	HitIDs          []string  `json:"hit_ids"`
	AggregationKey  string    `json:"aggregation_key"`
	AggregationRule string    `json:"aggregation_rule"`
	Timezone        string    `json:"timezone,omitempty"`
	Retroactive     bool      `json:"retroactive"`
	OpenedAt        time.Time `json:"opened_at"`
}

// HitsLinked 向既有案件追加命中关联，绝不覆盖已有命中。
type HitsLinked struct {
	CaseID      string    `json:"case_id"`
	HitIDs      []string  `json:"hit_ids"`
	Reason      string    `json:"reason"` // new_hit / duplicate / retroactive_rescan
	Retroactive bool      `json:"retroactive"`
	At          time.Time `json:"at"`
}

// CasesMerged 只追加一条关系：被吸收案件的原始命中与历史完整保留。
type CasesMerged struct {
	SurvivorCaseID string    `json:"survivor_case_id"`
	AbsorbedCaseID string    `json:"absorbed_case_id"`
	OfficerID      string    `json:"officer_id"`
	At             time.Time `json:"at"`
}

type CaseAssigned struct {
	CaseID    string    `json:"case_id"`
	OfficerID string    `json:"officer_id"`
	At        time.Time `json:"at"`
}

type CasePaused struct {
	CaseID   string    `json:"case_id"`
	Reason   string    `json:"reason"`
	By       string    `json:"by"`
	At       time.Time `json:"at"`
}

type CaseResumed struct {
	CaseID string    `json:"case_id"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
}

// CaseEscalated 升级时限到期仍未解除。
type CaseEscalated struct {
	CaseID   string    `json:"case_id"`
	Deadline time.Time `json:"deadline"`
	At       time.Time `json:"at"`
}

// CaseReopened 名单重新发布后回溯命中提供了新版本证据，已误报关闭的案件重新打开，
// SLA 从重新打开时刻重新起算（新的提醒纪元）。
type CaseReopened struct {
	CaseID      string    `json:"case_id"`
	Reason      string    `json:"reason"` // retroactive_rescan
	HitIDs      []string  `json:"hit_ids"`
	Epoch       int       `json:"epoch"` // 提醒纪元，每次重开递增
	At          time.Time `json:"at"`
}

// ReminderFired 提醒实际推动的记录；恢复后按 DueAt 顺序补发且幂等。
// Epoch 标识提醒纪元：案件重开后旧纪元的提醒即使到期也不得再发。
type ReminderFired struct {
	CaseID    string    `json:"case_id"`
	Epoch     int       `json:"epoch"`
	Milestone string    `json:"milestone"` // warning / escalation
	DueAt     time.Time `json:"due_at"`
	FiredAt   time.Time `json:"fired_at"`
}

type NoteRecorded struct {
	Note Note `json:"note"`
}

type DispositionRecorded struct {
	Disposition Disposition `json:"disposition"`
}

// EventTypes 事件类型注册表，供 JSONL 持久化与跨进程审计重放使用。
func EventTypes() map[string]any {
	return map[string]any{
		"customer.upserted":      &CustomerUpserted{},
		"officer.provisioned":    &OfficerProvisioned{},
		"list.published":         &ListPublished{},
		"list.withdrawn":         &ListWithdrawn{},
		"list.republished":       &ListRepublished{},
		"hit.recorded":           &HitRecorded{},
		"case.opened":            &CaseOpened{},
		"hits.linked":            &HitsLinked{},
		"cases.merged":           &CasesMerged{},
		"case.assigned":          &CaseAssigned{},
		"case.paused":            &CasePaused{},
		"case.resumed":           &CaseResumed{},
		"case.reopened":          &CaseReopened{},
		"case.escalated":         &CaseEscalated{},
		"reminder.fired":         &ReminderFired{},
		"note.recorded":          &NoteRecorded{},
		"disposition.recorded":   &DispositionRecorded{},
	}
}

// ---------------------------------------------------------------------------
// 错误
// ---------------------------------------------------------------------------

var (
	ErrCustomerNotFound      = errors.New("customer not found")
	ErrListNotFound          = errors.New("sanctions list not found")
	ErrListAlreadyExists     = errors.New("sanctions list already exists")
	ErrListVersionNotFound   = errors.New("list version not found")
	ErrListVersionInactive   = errors.New("list version is not active")
	ErrListNotWithdrawn      = errors.New("list is not withdrawn")
	ErrCaseNotFound          = errors.New("case not found")
	ErrCaseAbsorbed          = errors.New("case has been merged into another case; act on the root case")
	ErrCaseTerminal          = errors.New("case is already resolved")
	ErrCasePaused            = errors.New("case is paused; resume before changing its state")
	ErrCaseNotPaused         = errors.New("case is not paused")
	ErrCasesAlreadyRelated   = errors.New("cases are already related by a merge")
	ErrReviewerRequired      = errors.New("reviewer is required for any disposition")
	ErrEvidenceRequired      = errors.New("evidence summary is required for any disposition")
	ErrDecisionInvalid       = errors.New("invalid disposition decision")
	ErrOfficerNotFound       = errors.New("officer not found in roster")
	ErrRoleNotPermitted      = errors.New("actor role is not permitted to perform this action")
	ErrInvalidInput          = errors.New("invalid input")
)

// itoa 避免在热点 BusinessKey 上引入 strconv 的局部小工具。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
