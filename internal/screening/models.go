package screening

import "time"

// Customer 客户主数据。Aliases 是已知别名（同一人的多个姓名形式），
// 若同一人以多个别名同时命中，应通过别名关系聚合到同一案件。
type Customer struct {
	ID           string    `json:"id"`
	LegalName    string    `json:"legalName"`
	Aliases      []string  `json:"aliases,omitempty"`
	NationalID   string    `json:"nationalId,omitempty"`
	Address      string    `json:"address,omitempty"` // 可能缺失（空串）
	Jurisdiction string    `json:"jurisdiction,omitempty"`
	UpdatedAt    time.Time `json:"updatedAt,omitempty"`
}

// ListEntry 制裁名单条目。名单以版本化方式发布；撤回后可用新版本重新发布（可能修正姓名等）。
type ListEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Aliases     []string `json:"aliases,omitempty"`
	Program     string `json:"program,omitempty"`     // 制裁项目，如 OFAC SDN
	Nationality string `json:"nationality,omitempty"`
	Version     string `json:"version"`
	Active      bool   `json:"active"` // false 表示该版本已被撤回
}

// Hit 筛查命中（来自批量筛查结果）。
type Hit struct {
	ID          string    `json:"id"`
	BatchID     string    `json:"batchId,omitempty"`
	CustomerID  string    `json:"customerId,omitempty"`
	RawName     string    `json:"rawName"` // 原始命中姓名（低权限客服不可见）
	Address     string    `json:"address,omitempty"` // 可能缺失
	NationalID  string    `json:"nationalId,omitempty"`
	ListEntryID string    `json:"listEntryId"`
	ListVersion string    `json:"listVersion"` // 命中时所依据的名单版本
	Score       float64   `json:"score,omitempty"`
	ScreenedAt  time.Time `json:"screenedAt,omitempty"`
}

// Disposition 命中/案件处置结论。
type Disposition string

const (
	DispPending          Disposition = "PENDING"                // 待调查
	DispFalsePositive    Disposition = "FALSE_POSITIVE"         // 误报
	DispTruePositive     Disposition = "TRUE_POSITIVE"          // 真阳性
	DispDuplicate        Disposition = "DUPLICATE"              // 重复命中
	DispRetroactiveClear Disposition = "LIST_WITHDRAWN_CLEARED" // 名单回溯撤回导致解除
	DispEscalated        Disposition = "ESCALATED"              // 已升级
	DispMerged           Disposition = "MERGED"                 // 已并入其他案件（追加关系，原案件材料保留）
)

// Case 案件是聚合根。案件合并只能追加关系（SourceCases），
// 原命中与被并案件的材料绝不被覆盖或删除。
type Case struct {
	ID           string            `json:"id"`
	HitIDs       []string          `json:"hitIds"`              // 案件内全部命中（含合并带入的）
	SourceCases  []string          `json:"sourceCases,omitempty"` // 合并来源案件（追加，不去重覆盖）
	CustomerID   string            `json:"customerId,omitempty"`
	CustomerName string            `json:"customerName,omitempty"` // 主数据上的法定姓名（展示兜底）
	Status       Disposition       `json:"status"`
	Assignee     string            `json:"assignee,omitempty"`
	Suspended    bool              `json:"suspended"`
	SuspendReason string           `json:"suspendReason,omitempty"`
	Reviewer     string            `json:"reviewer,omitempty"` // 最近一次解除动作引用的复核人
	Evidence     string            `json:"evidence,omitempty"` // 证据摘要
	Rules        []string          `json:"rules,omitempty"`    // 聚合/匹配规则说明（可解释性）
	ListVersions map[string]string `json:"listVersions,omitempty"` // listEntryID -> 影响本案的最新名单版本
	CreatedAt    time.Time         `json:"createdAt"`
	UpdatedAt    time.Time         `json:"updatedAt"`
}

// InvestigationNote 调查笔记。
type InvestigationNote struct {
	ID        string    `json:"id"`
	CaseID    string    `json:"caseId"`
	Author    string    `json:"author"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"createdAt"`
}

// DueReminder 待推动的提醒/升级。顺序键为 OccurredAt（发生顺序），
// 跨时区截止时间与故障恢复后均按此顺序补发，不重不漏。
type DueReminder struct {
	Key        string    `json:"key"`
	CaseID     string    `json:"caseId"`
	OccurredAt time.Time `json:"occurredAt"` // 触发时刻（升级时限到达的绝对时刻，按时区换算后存储）
	Kind       string    `json:"kind"`       // "reminder" | "escalation"
	Fired      bool      `json:"fired"`
}
