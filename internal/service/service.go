// Package service 是案件台的应用层：把外部输入（批量命中、名单版本、客户主数据、
// 调查笔记）翻译为仅追加的领域事件，并从事件投影回答查询。
//
// 关键语义：
//   - 所有写操作在同一把命令锁内串行化，保证「同一客户+同一名单」的并发批次
//     不会开出两个案件；事件落盘即提交，崩溃后靠重放恢复；
//   - 聚合键以客户号为边界：姓名再相似、地址再缺失，两个客户也不会并案；
//   - 案件合并只追加 CasesMerged 关系，原始命中保留在原案件上；
//   - SLA 截止时间是绝对时刻（UTC 存储），展示时换算案件时区；暂停冻结 SLA；
//   - 名单重发触发回溯重扫，曾以误报关闭的案件凭新版本证据重开并进入新提醒纪元；
//   - 任何解除都强制引用复核人与证据摘要。
package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	// 内嵌 IANA 时区数据库，保证精简容器中跨时区截止时间仍可换算。
	_ "time/tzdata"

	"example.com/09181/q012/internal/domain"
	"example.com/09181/q012/internal/match"
	"example.com/09181/q012/internal/store"
)

const (
	// AggregationRuleCustomerList 案件聚合规则（可解释）：
	// 同一客户主体在同一名单下的全部命中归入同一案件，无论命中来自哪个姓名/别名、
	// 哪个版本或哪次回溯。跨客户绝不聚合。
	AggregationRuleCustomerList = "same_customer_same_list"

	linkNewHit      = "new_hit"
	linkDuplicate   = "duplicate"
	linkRetroactive = "retroactive_rescan"
)

// Clock 注入时间源，测试可替换。
type Clock func() time.Time

// Reminder 是推送给外部通道（邮件/工单）的提醒负载。
type Reminder struct {
	CaseID     string    `json:"case_id"`
	Epoch      int       `json:"epoch"`
	Milestone  string    `json:"milestone"`
	DueAt      time.Time `json:"due_at"`      // 绝对到期时刻
	LocalDueAt time.Time `json:"local_due_at"` // 换算到案件时区后的本地到期时刻
	Timezone   string    `json:"timezone"`
}

// ReminderSink 接收提醒；事件已先于投递落盘，投递失败不影响状态正确性。
type ReminderSink interface {
	Deliver(ctx context.Context, r Reminder) error
}

// Config 服务可调参数。
type Config struct {
	MatcherThreshold float64
	SLA              time.Duration // 从案件锚点（开立/重开）到必须解除的时长
	WarningLead      time.Duration // 升级前提前多久发预警
	Clock            Clock
	Sink             ReminderSink
}

// Service 案件台应用服务。
type Service struct {
	store   *store.EventStore
	proj    *store.Projection
	matcher *match.Matcher
	sink    ReminderSink

	cmdMu sync.Mutex // 串行化全部命令，杜绝并发下的重复开案/重复合并
	clock Clock
	sla   time.Duration
	warn  time.Duration

	idMu      sync.Mutex
	hitSeq    int
	caseSeq   int
	undelivMu sync.Mutex
	undeliv   []Reminder
}

// New 构造服务并重放事件日志恢复全部状态（含提醒幂等记录与暂停累计）。
func New(es *store.EventStore, cfg Config) (*Service, error) {
	if cfg.SLA <= 0 {
		cfg.SLA = 24 * time.Hour
	}
	if cfg.WarningLead <= 0 {
		cfg.WarningLead = 4 * time.Hour
	}
	clock := cfg.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	s := &Service{
		store:   es,
		proj:    store.NewProjection(),
		matcher: match.New(cfg.MatcherThreshold),
		sink:    cfg.Sink,
		clock:   clock,
		sla:     cfg.SLA,
		warn:    cfg.WarningLead,
	}
	// 订阅即先回放历史；完成后计数器从历史事件总数推导（含已被吸收的案件）。
	if err := es.Subscribe(s.proj.Apply); err != nil {
		return nil, err
	}
	counts := struct{ hits, cases int }{}
	if err := es.Replay(func(_ int64, typ string, _ any) error {
		switch typ {
		case "hit.recorded":
			counts.hits++
		case "case.opened":
			counts.cases++
		}
		return nil
	}); err != nil {
		return nil, err
	}
	s.hitSeq = counts.hits
	s.caseSeq = counts.cases
	return s, nil
}

// Projection 暴露只读投影（HTTP 层与测试使用）。
func (s *Service) Projection() *store.Projection { return s.proj }

func (s *Service) now() time.Time { return s.clock().UTC() }

// ---------------------------------------------------------------------------
// 人员名册
// ---------------------------------------------------------------------------

// RegisterOfficer 登记客服/合规官/复核人。
func (s *Service) RegisterOfficer(o domain.Officer) error {
	if strings.TrimSpace(o.OfficerID) == "" || strings.TrimSpace(o.Name) == "" {
		return fmt.Errorf("%w: officer id and name are required", domain.ErrInvalidInput)
	}
	switch o.Role {
	case domain.RoleAgent, domain.RoleOfficer, domain.RoleReviewer:
	default:
		return fmt.Errorf("%w: unknown role %q", domain.ErrInvalidInput, o.Role)
	}
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	if _, ok := s.proj.Officer(o.OfficerID); ok {
		return fmt.Errorf("%w: officer %s", domain.ErrInvalidInput, o.OfficerID)
	}
	_, err := s.store.Append("officer.provisioned", domain.OfficerProvisioned{Officer: o, At: s.now()})
	return err
}

// actor 解析并校验操作人。
func (s *Service) actor(officerID string) (domain.Officer, error) {
	o, ok := s.proj.Officer(officerID)
	if !ok {
		return domain.Officer{}, domain.ErrOfficerNotFound
	}
	if o.Disabled {
		return domain.Officer{}, domain.ErrOfficerNotFound
	}
	return o, nil
}

func requireRole(o domain.Officer, roles ...domain.Role) error {
	for _, r := range roles {
		if o.Role == r {
			return nil
		}
	}
	return domain.ErrRoleNotPermitted
}

// ---------------------------------------------------------------------------
// 客户主数据
// ---------------------------------------------------------------------------

// UpsertCustomer 接收客户主数据（姓名、别名、缺失地址等）。
func (s *Service) UpsertCustomer(c domain.Customer) error {
	if strings.TrimSpace(c.CustomerID) == "" || strings.TrimSpace(c.LegalName) == "" {
		return fmt.Errorf("%w: customer_id and legal_name are required", domain.ErrInvalidInput)
	}
	if c.Timezone != "" {
		if _, err := time.LoadLocation(c.Timezone); err != nil {
			return fmt.Errorf("%w: timezone %q: %v", domain.ErrInvalidInput, c.Timezone, err)
		}
	}
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	_, err := s.store.Append("customer.upserted", domain.CustomerUpserted{Customer: c, At: s.now()})
	return err
}

// ---------------------------------------------------------------------------
// 名单版本：发布 / 撤回 / 重新发布（回溯）
// ---------------------------------------------------------------------------

// PublishList 首次发布名单（仅用于此前不存在的名单）。
// 撤回后的重新发布必须走 RepublishList，以保证触发回溯重扫。
func (s *Service) PublishList(listID string, version int, entries []domain.ListEntry, at time.Time) error {
	if strings.TrimSpace(listID) == "" || version <= 0 || len(entries) == 0 {
		return fmt.Errorf("%w: list_id, positive version and entries are required", domain.ErrInvalidInput)
	}
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()

	if l, ok := s.proj.List(listID); ok {
		return fmt.Errorf("%w: list %s already exists (%d versions); withdraw and use republish for a new version",
			domain.ErrListAlreadyExists, listID, len(l.ListVersions()))
	}
	return s.appendListPublished(listID, version, entries, at)
}

func (s *Service) appendListPublished(listID string, version int, entries []domain.ListEntry, at time.Time) error {
	if at.IsZero() {
		at = s.now()
	}
	_, err := s.store.Append("list.published", domain.ListPublished{
		ListID: listID, Version: version, Entries: cloneEntries(entries), PublishedAt: at.UTC(),
	})
	return err
}

// WithdrawList 撤回现行版本；撤回后不再产生实时命中，旧版本数据保留备查。
func (s *Service) WithdrawList(listID string, version int) error {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	v, ok, err := s.versionState(listID, version)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s v%d", domain.ErrListVersionNotFound, listID, version)
	}
	if v.Status != domain.ListActive {
		return fmt.Errorf("%w: %s v%d already withdrawn", domain.ErrListVersionInactive, listID, version)
	}
	_, err = s.store.Append("list.withdrawn", domain.ListWithdrawn{
		ListID: listID, Version: version, WithdrawnAt: s.now(),
	})
	return err
}

// RepublishList 撤回后的名单以新版本重新发布，立即对存量客户回溯重扫。
// 回溯产生的命中、关联、案件重开与发布事件具有确定的先后顺序。
func (s *Service) RepublishList(listID string, baseVersion, newVersion int, entries []domain.ListEntry, at time.Time) (RetroactiveReport, error) {
	if strings.TrimSpace(listID) == "" || newVersion <= 0 || len(entries) == 0 {
		return RetroactiveReport{}, fmt.Errorf("%w: list_id, positive version and entries are required", domain.ErrInvalidInput)
	}
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()

	l, ok := s.proj.List(listID)
	if !ok {
		return RetroactiveReport{}, fmt.Errorf("%w: %s", domain.ErrListNotFound, listID)
	}
	base, exists := l.ListVersions()[baseVersion]
	if !exists {
		return RetroactiveReport{}, fmt.Errorf("%w: base %s v%d", domain.ErrListVersionNotFound, listID, baseVersion)
	}
	if base.Status != domain.ListWithdrawn {
		return RetroactiveReport{}, fmt.Errorf("%w: base version %d must be withdrawn first", domain.ErrListNotWithdrawn, baseVersion)
	}
	maxV := 0
	for n := range l.ListVersions() {
		if n > maxV {
			maxV = n
		}
	}
	if newVersion <= maxV {
		return RetroactiveReport{}, fmt.Errorf("%w: new version %d must exceed %d", domain.ErrInvalidInput, newVersion, maxV)
	}
	if at.IsZero() {
		at = s.now()
	}
	if _, err := s.store.Append("list.republished", domain.ListRepublished{
		ListID: listID, Version: newVersion, BaseVersion: baseVersion,
		Entries: cloneEntries(entries), At: at.UTC(),
	}); err != nil {
		return RetroactiveReport{}, err
	}
	return s.runRetroactiveRescanLocked(listID, newVersion, entries, at)
}

func (s *Service) versionState(listID string, version int) (*store.ListVersionState, bool, error) {
	l, ok := s.proj.List(listID)
	if !ok {
		return nil, false, fmt.Errorf("%w: %s", domain.ErrListNotFound, listID)
	}
	v, ok := l.ListVersions()[version]
	return v, ok, nil
}

// ---------------------------------------------------------------------------
// 筛查与批量命中接入
// ---------------------------------------------------------------------------

// HitInput 是一批外部筛查命中中的一条；姓名使用客户侧原始姓名（别名照原样保留）。
type HitInput struct {
	CustomerID  string
	SubjectKey  string // 客户号缺失时的显式主体标识；缺失则不与任何命中聚合
	ListID      string
	Version     int // 0 表示由服务取名单现行版本（实时命中）
	EntryID     string
	MatchedName string
	Score       float64
	Reasons     []domain.MatchReason
	Origin      domain.HitOrigin // 0 视为 live
	OccurredAt  time.Time
}

// IngestReport 汇总一批命中的落点。
type IngestReport struct {
	HitIDs          []string `json:"hit_ids"`
	CaseIDs         []string `json:"case_ids"` // 每条命中最终归属的根案件（按 HitIDs 对齐）
	OpenedCaseIDs   []string `json:"opened_case_ids"`
	ReopenedCaseIDs []string `json:"reopened_case_ids"`
	Duplicates      int      `json:"duplicates"`
}

// RetroactiveReport 回溯重扫结果。
type RetroactiveReport struct {
	ListID          string   `json:"list_id"`
	Version         int      `json:"version"`
	HitIDs          []string `json:"hit_ids"`
	CaseIDs         []string `json:"case_ids"`
	OpenedCaseIDs   []string `json:"opened_case_ids"`
	ReopenedCaseIDs []string `json:"reopened_case_ids"`
}

// IngestHits 接收外部筛查引擎产生的批量命中（实时）。
// 命中只记录在名单现行版本上；重复命中按业务键识别并显式标注。
func (s *Service) IngestHits(inputs []HitInput) (IngestReport, error) {
	if len(inputs) == 0 {
		return IngestReport{}, fmt.Errorf("%w: empty batch", domain.ErrInvalidInput)
	}
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()

	normalized := make([]HitInput, len(inputs))
	for i, in := range inputs {
		if err := validateHitInput(in); err != nil {
			return IngestReport{}, err
		}
		l, ok := s.proj.List(in.ListID)
		if !ok {
			return IngestReport{}, fmt.Errorf("%w: %s", domain.ErrListNotFound, in.ListID)
		}
		version := l.ActiveVersion()
		if version == 0 {
			return IngestReport{}, fmt.Errorf("%w: %s has no active version", domain.ErrListVersionInactive, in.ListID)
		}
		if _, exists := l.ListVersions()[version].EntryByID(in.EntryID); !exists {
			return IngestReport{}, fmt.Errorf("%w: entry %q not in %s v%d", domain.ErrInvalidInput, in.EntryID, in.ListID, version)
		}
		in.Version = version
		if in.Origin == "" {
			in.Origin = domain.OriginLive
		}
		normalized[i] = in
	}
	return s.recordHitsLocked(normalized)
}

// ScreenCustomers 用内置可解释匹配引擎对客户主数据做实时筛查，
// 覆盖每份名单的现行版本；返回落点报告（命中同时已入账）。
func (s *Service) ScreenCustomers(customerIDs []string) (IngestReport, error) {
	if len(customerIDs) == 0 {
		return IngestReport{}, fmt.Errorf("%w: no customers given", domain.ErrInvalidInput)
	}
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()

	var inputs []HitInput
	now := s.now()
	for _, cid := range customerIDs {
		c, ok := s.proj.Customer(cid)
		if !ok {
			return IngestReport{}, fmt.Errorf("%w: customer %s", domain.ErrCustomerNotFound, cid)
		}
		cand := match.FromCustomer(c)
		for _, l := range s.proj.AllLists() {
			version := l.ActiveVersion()
			if version == 0 {
				continue
			}
			st := l.ListVersions()[version]
			for _, r := range s.matcher.Screen(cand, st.Entries) {
				if !r.Match {
					continue
				}
				inputs = append(inputs, HitInput{
					CustomerID: c.CustomerID, ListID: l.ListID, Version: version,
					EntryID: r.Entry.EntryID, MatchedName: r.MatchedName,
					Score: r.Score, Reasons: r.Reasons, OccurredAt: now,
				})
			}
		}
	}
	if len(inputs) == 0 {
		return IngestReport{}, nil
	}
	for i := range inputs {
		if inputs[i].Origin == "" {
			inputs[i].Origin = domain.OriginLive
		}
	}
	return s.recordHitsLocked(inputs)
}

func validateHitInput(in HitInput) error {
	if strings.TrimSpace(in.ListID) == "" || strings.TrimSpace(in.EntryID) == "" ||
		strings.TrimSpace(in.MatchedName) == "" {
		return fmt.Errorf("%w: list_id, entry_id and matched_name are required", domain.ErrInvalidInput)
	}
	if in.Score < 0 || in.Score > 1 {
		return fmt.Errorf("%w: score must be within [0,1]", domain.ErrInvalidInput)
	}
	return nil
}

// runRetroactiveRescanLocked 对全部存量客户用新版本重扫并记录回溯命中。
func (s *Service) runRetroactiveRescanLocked(listID string, version int, entries []domain.ListEntry, at time.Time) (RetroactiveReport, error) {
	rep := RetroactiveReport{ListID: listID, Version: version}
	var inputs []HitInput
	for _, c := range s.proj.Customers() {
		cand := match.FromCustomer(c)
		for _, r := range s.matcher.Screen(cand, entries) {
			if !r.Match {
				continue
			}
			inputs = append(inputs, HitInput{
				CustomerID: c.CustomerID, ListID: listID, Version: version,
				EntryID: r.Entry.EntryID, MatchedName: r.MatchedName,
				Score: r.Score, Reasons: r.Reasons, OccurredAt: at,
				Origin: domain.OriginRetroactive,
			})
		}
	}
	if len(inputs) == 0 {
		return rep, nil
	}
	r, err := s.recordHitsLocked(inputs)
	if err != nil {
		return rep, err
	}
	rep.HitIDs = r.HitIDs
	rep.CaseIDs = r.CaseIDs
	rep.OpenedCaseIDs = r.OpenedCaseIDs
	rep.ReopenedCaseIDs = r.ReopenedCaseIDs
	return rep, nil
}

// recordHitsLocked 统一落点：去重 → 聚合键找案件 → 开案/关联/重开。
// 调用方必须持有 cmdMu。
func (s *Service) recordHitsLocked(inputs []HitInput) (IngestReport, error) {
	rep := IngestReport{
		HitIDs:  make([]string, len(inputs)),
		CaseIDs: make([]string, len(inputs)),
	}
	// 确定性处理顺序：同批内稳定，不因 map 遍历产生不同事件序列。
	order := make([]int, len(inputs))
	for i := range inputs {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return hitSortKey(inputs[order[a]]) < hitSortKey(inputs[order[b]])
	})

	for _, idx := range order {
		in := inputs[idx]
		at := in.OccurredAt.UTC()
		if at.IsZero() {
			at = s.now()
		}
		hitID := s.nextHitID()
		version := in.Version

		origin := in.Origin
		if origin == "" {
			origin = domain.OriginLive
		}
		h := domain.Hit{
			HitID: hitID, CustomerID: in.CustomerID, ListID: in.ListID,
			ListVersion: version, EntryID: in.EntryID, MatchedName: in.MatchedName,
			Score: in.Score, Reasons: append([]domain.MatchReason{}, in.Reasons...),
			Origin: origin, OccurredAt: at,
		}
		key := s.aggregationKey(in, hitID)

		// 历史去重（含本批已入账的前一条，事件同步更新投影）：显式标注为重复命中。
		// 无客户号/主体标识的游离命中不具备同一性前提，即使字段相同也不判重，
		// 以免把两个无法确认的主体错误关联。
		identified := strings.TrimSpace(in.CustomerID) != "" || strings.TrimSpace(in.SubjectKey) != ""
		if identified {
			if existing, ok := s.proj.HitByKey(h.BusinessKey()); ok {
				h.DuplicateOf = existing.HitID
				rep.Duplicates++
			}
		}

		rootID, root := s.proj.CaseByAggregationKey(key)
		if rootID == "" {
			caseID := s.nextCaseID()
			h.CaseID = caseID
			if _, err := s.store.Append("hit.recorded", domain.HitRecorded{Hit: h}); err != nil {
				return rep, err
			}
			tz := s.customerTimezone(in.CustomerID)
			if _, err := s.store.Append("case.opened", domain.CaseOpened{
				CaseID: caseID, CustomerID: in.CustomerID, HitIDs: []string{hitID},
				AggregationKey: key, AggregationRule: AggregationRuleCustomerList,
				Timezone: tz, Retroactive: h.Origin == domain.OriginRetroactive,
				OpenedAt: at,
			}); err != nil {
				return rep, err
			}
			rootID = caseID
			rep.OpenedCaseIDs = append(rep.OpenedCaseIDs, caseID)
		} else {
			h.CaseID = rootID
			if _, err := s.store.Append("hit.recorded", domain.HitRecorded{Hit: h}); err != nil {
				return rep, err
			}
			linkReason := linkNewHit
			if h.DuplicateOf != "" {
				linkReason = linkDuplicate
			} else if h.Origin == domain.OriginRetroactive {
				linkReason = linkRetroactive
			}

			// 回溯带来版本化的新证据（非纯重复）且案件曾以误报关闭：重开，SLA 重新起算。
			if h.Origin == domain.OriginRetroactive && h.DuplicateOf == "" &&
				root.Status == domain.StatusFalsePositive {
				epoch := root.Epoch + 1
				if _, err := s.store.Append("case.reopened", domain.CaseReopened{
					CaseID: rootID, Reason: linkRetroactive, HitIDs: []string{hitID},
					Epoch: epoch, At: at,
				}); err != nil {
					return rep, err
				}
				rep.ReopenedCaseIDs = appendUnique(rep.ReopenedCaseIDs, rootID)
			} else {
				if _, err := s.store.Append("hits.linked", domain.HitsLinked{
					CaseID: rootID, HitIDs: []string{hitID}, Reason: linkReason,
					Retroactive: h.Origin == domain.OriginRetroactive, At: at,
				}); err != nil {
					return rep, err
				}
			}
		}
		rep.HitIDs[idx] = hitID
		rep.CaseIDs[idx] = rootID
	}
	rep.OpenedCaseIDs = sortedUnique(rep.OpenedCaseIDs)
	rep.ReopenedCaseIDs = sortedUnique(rep.ReopenedCaseIDs)
	return rep, nil
}

func (s *Service) aggregationKey(in HitInput, hitID string) string {
	if strings.TrimSpace(in.CustomerID) != "" {
		return "cust:" + in.CustomerID + "|list:" + in.ListID
	}
	if k := strings.TrimSpace(in.SubjectKey); k != "" {
		return "subj:" + k + "|list:" + in.ListID
	}
	// 无客户号且无主体标识：绝不与其他命中并案，防止把两个客户的材料放进同一案件。
	return "hit:" + hitID
}

func (s *Service) customerTimezone(customerID string) string {
	if customerID == "" {
		return ""
	}
	if c, ok := s.proj.Customer(customerID); ok {
		return c.Timezone
	}
	return ""
}

// ---------------------------------------------------------------------------
// 案件动作：分配 / 合并 / 暂停 / 笔记 / 解除
// ---------------------------------------------------------------------------

// AssignCase 把案件分配给合规官。
func (s *Service) AssignCase(actorID, caseID, toOfficerID string) error {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	actor, err := s.actor(actorID)
	if err != nil {
		return err
	}
	if err := requireRole(actor, domain.RoleOfficer, domain.RoleReviewer); err != nil {
		return err
	}
	target, err := s.actor(toOfficerID)
	if err != nil {
		return err
	}
	if err := requireRole(target, domain.RoleOfficer, domain.RoleReviewer); err != nil {
		return fmt.Errorf("%w: assignee %s must be an officer or reviewer", err, toOfficerID)
	}
	rootID, root, err := s.mustRoot(caseID)
	if err != nil {
		return err
	}
	if err := requireMutable(root); err != nil {
		return err
	}
	_, err = s.store.Append("case.assigned", domain.CaseAssigned{
		CaseID: rootID, OfficerID: toOfficerID, At: s.now(),
	})
	return err
}

// MergeCases 只追加合并关系：survivor 保留，absorbed 标记 merged_away，
// 双方原始命中与历史原样保留。不得重复合并、不得跨已存在的合并树。
func (s *Service) MergeCases(officerID, survivorID, absorbedID string) error {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	actor, err := s.actor(officerID)
	if err != nil {
		return err
	}
	if err := requireRole(actor, domain.RoleOfficer, domain.RoleReviewer); err != nil {
		return err
	}
	survivorRoot, survivor, err := s.mustRoot(survivorID)
	if err != nil {
		return err
	}
	absorbedRoot, absorbed, err := s.mustRoot(absorbedID)
	if err != nil {
		return err
	}
	if survivorRoot == absorbedRoot {
		return domain.ErrCasesAlreadyRelated
	}
	if err := requireMutable(survivor); err != nil {
		return err
	}
	if err := requireMutable(absorbed); err != nil {
		return err
	}
	// 合并树已经相交则拒绝（A 并入 B 后不能再把 B 并入 A 的后代）。
	if related(s.proj, survivorRoot, absorbedRoot) {
		return domain.ErrCasesAlreadyRelated
	}
	_, err = s.store.Append("cases.merged", domain.CasesMerged{
		SurvivorCaseID: survivorRoot, AbsorbedCaseID: absorbedRoot,
		OfficerID: officerID, At: s.now(),
	})
	return err
}

// PauseCase 暂停案件，SLA 时钟同时冻结。
func (s *Service) PauseCase(actorID, caseID, reason string) error {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	actor, err := s.actor(actorID)
	if err != nil {
		return err
	}
	if err := requireRole(actor, domain.RoleOfficer, domain.RoleReviewer); err != nil {
		return err
	}
	rootID, root, err := s.mustRoot(caseID)
	if err != nil {
		return err
	}
	if root.Status.Terminal() {
		return domain.ErrCaseTerminal
	}
	if root.Paused {
		return domain.ErrCasePaused
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: pause reason is required", domain.ErrInvalidInput)
	}
	_, err = s.store.Append("case.paused", domain.CasePaused{
		CaseID: rootID, Reason: reason, By: actorID, At: s.now(),
	})
	return err
}

// ResumeCase 恢复案件，暂停时长计入 SLA 顺延。
func (s *Service) ResumeCase(actorID, caseID string) error {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	actor, err := s.actor(actorID)
	if err != nil {
		return err
	}
	if err := requireRole(actor, domain.RoleOfficer, domain.RoleReviewer); err != nil {
		return err
	}
	rootID, root, err := s.mustRoot(caseID)
	if err != nil {
		return err
	}
	if root.Status.Terminal() {
		return domain.ErrCaseTerminal
	}
	if !root.Paused {
		return domain.ErrCaseNotPaused
	}
	_, err = s.store.Append("case.resumed", domain.CaseResumed{
		CaseID: rootID, By: actorID, At: s.now(),
	})
	return err
}

// AddNote 追加调查笔记（只增不改）。
func (s *Service) AddNote(actorID, caseID, content string) error {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	actor, err := s.actor(actorID)
	if err != nil {
		return err
	}
	if err := requireRole(actor, domain.RoleOfficer, domain.RoleReviewer); err != nil {
		return err
	}
	rootID, _, err := s.mustRoot(caseID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("%w: note content is required", domain.ErrInvalidInput)
	}
	_, err = s.store.Append("note.recorded", domain.NoteRecorded{Note: domain.Note{
		CaseID: rootID, AuthorID: actorID, Content: content, CreatedAt: s.now(),
	}})
	return err
}

// DispositionInput 解除请求。
type DispositionInput struct {
	CaseID            string
	Decision          domain.Decision
	EvidenceSummary   string
	DuplicateOfCaseID string
}

// ResolveCase 由复核人作出解除：误报 / 真阳性 / 重复命中。
// 任何解除都必须引用复核人与证据摘要；重复命中还必须引用被重复的案件。
func (s *Service) ResolveCase(reviewerID string, in DispositionInput) error {
	if !in.Decision.Valid() {
		return domain.ErrDecisionInvalid
	}
	if strings.TrimSpace(in.EvidenceSummary) == "" {
		return domain.ErrEvidenceRequired
	}
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()

	reviewer, err := s.actor(reviewerID)
	if err != nil {
		return err
	}
	if err := requireRole(reviewer, domain.RoleReviewer); err != nil {
		return err
	}
	rootID, root, err := s.mustRoot(in.CaseID)
	if err != nil {
		return err
	}
	if root.Status.Terminal() {
		return domain.ErrCaseTerminal
	}
	if root.Paused {
		return domain.ErrCasePaused
	}
	dupRef := ""
	if in.Decision == domain.DecisionDuplicate {
		if strings.TrimSpace(in.DuplicateOfCaseID) == "" {
			return fmt.Errorf("%w: duplicate disposition must reference the original case", domain.ErrInvalidInput)
		}
		refRoot, _, ok := s.proj.RootCase(in.DuplicateOfCaseID)
		if !ok {
			return fmt.Errorf("%w: referenced case %s", domain.ErrCaseNotFound, in.DuplicateOfCaseID)
		}
		if refRoot == rootID {
			return fmt.Errorf("%w: a case cannot be a duplicate of itself", domain.ErrInvalidInput)
		}
		dupRef = refRoot
	}
	_, err = s.store.Append("disposition.recorded", domain.DispositionRecorded{Disposition: domain.Disposition{
		CaseID: rootID, ReviewerID: reviewerID, Decision: in.Decision,
		EvidenceSummary: strings.TrimSpace(in.EvidenceSummary),
		DuplicateOfCaseID: dupRef, CreatedAt: s.now(),
	}})
	return err
}

func (s *Service) mustRoot(caseID string) (string, *store.CaseState, error) {
	rootID, root, ok := s.proj.RootCase(caseID)
	if !ok {
		return "", nil, domain.ErrCaseNotFound
	}
	if rootID != caseID {
		return "", nil, fmt.Errorf("%w: %s -> %s", domain.ErrCaseAbsorbed, caseID, rootID)
	}
	return rootID, root, nil
}

// requireMutable 拒绝终态与暂停中的案件执行结构/状态变更。
func requireMutable(c *store.CaseState) error {
	if c.Status.Terminal() {
		return domain.ErrCaseTerminal
	}
	if c.Paused {
		return domain.ErrCasePaused
	}
	return nil
}

// related 判断两个根案件是否已在同一合并树中。
func related(p *store.Projection, a, b string) bool {
	snap, ok := p.CaseSnapshot(a)
	if !ok {
		return false
	}
	for _, id := range snap.RelatedCases {
		if id == b {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// SLA 提醒泵
// ---------------------------------------------------------------------------

// DueInfo 暴露给查询层的到期信息（绝对时刻 + 案件时区本地时刻）。
type DueInfo struct {
	WarningAt    time.Time
	EscalationAt time.Time
	LocalWarning time.Time
	LocalEscalation time.Time
}

// CaseDeadline 计算案件当前纪元的预警/升级时刻（含暂停顺延），并换算本地时区。
func (s *Service) CaseDeadline(snap store.Snapshot) DueInfo {
	c := snap.Case
	warnAt := c.AnchorAt.Add(c.PausedTotal + s.sla - s.warn)
	escAt := c.AnchorAt.Add(c.PausedTotal + s.sla)
	loc := time.UTC
	if c.Timezone != "" {
		if l, err := time.LoadLocation(c.Timezone); err == nil {
			loc = l
		}
	}
	return DueInfo{
		WarningAt: warnAt, EscalationAt: escAt,
		LocalWarning: warnAt.In(loc), LocalEscalation: escAt.In(loc),
	}
}

// PumpReminders 推动所有已到期提醒：
//   - 按 DueAt 升序补发（跨时区只认绝对时刻），同一批内顺序确定；
//   - 每个 (案件, 纪元, 里程碑) 幂等一次，崩溃恢复后不会重复打扰；
//   - 升级里程碑同时把案件置为 escalated。
func (s *Service) PumpReminders(ctx context.Context) ([]Reminder, error) {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()

	// 先尝试投递上次崩溃前未落至外部通道的提醒（至少一次语义）。
	fired := s.drainUndelivered(ctx)

	now := s.now()
	for _, d := range s.proj.DueReminders(now, s.warn, s.sla) {
		snap, ok := s.proj.CaseSnapshot(d.CaseID)
		if !ok {
			continue
		}
		if d.Milestone == store.MilestoneEscalation && snap.Case.Status != domain.StatusEscalated {
			if _, err := s.store.Append("case.escalated", domain.CaseEscalated{
				CaseID: d.CaseID, Deadline: d.DueAt, At: now,
			}); err != nil {
				return fired, err
			}
		}
		if _, err := s.store.Append("reminder.fired", domain.ReminderFired{
			CaseID: d.CaseID, Epoch: d.Epoch, Milestone: d.Milestone,
			DueAt: d.DueAt, FiredAt: now,
		}); err != nil {
			return fired, err
		}
		r := s.toReminder(snap, d.Epoch, d.Milestone, d.DueAt)
		if err := s.deliver(ctx, r); err != nil {
			s.queueUndelivered(r)
		}
		fired = append(fired, r)
	}
	return fired, nil
}

func (s *Service) toReminder(snap store.Snapshot, epoch int, milestone string, due time.Time) Reminder {
	loc := time.UTC
	if tz := snap.Case.Timezone; tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	return Reminder{
		CaseID: snap.Case.CaseID, Epoch: epoch, Milestone: milestone,
		DueAt: due, LocalDueAt: due.In(loc), Timezone: snap.Case.Timezone,
	}
}

func (s *Service) deliver(ctx context.Context, r Reminder) error {
	if s.sink == nil {
		return nil
	}
	return s.sink.Deliver(ctx, r)
}

func (s *Service) queueUndelivered(r Reminder) {
	s.undelivMu.Lock()
	s.undeliv = append(s.undeliv, r)
	s.undelivMu.Unlock()
}

func (s *Service) drainUndelivered(ctx context.Context) []Reminder {
	s.undelivMu.Lock()
	pending := s.undeliv
	s.undeliv = nil
	s.undelivMu.Unlock()

	var delivered []Reminder
	var still []Reminder
	for _, r := range pending {
		if err := s.deliver(ctx, r); err != nil {
			still = append(still, r)
			continue
		}
		delivered = append(delivered, r)
	}
	if len(still) > 0 {
		s.undelivMu.Lock()
		s.undeliv = append(still, s.undeliv...)
		s.undelivMu.Unlock()
	}
	return delivered
}

// ---------------------------------------------------------------------------
// ID 生成
// ---------------------------------------------------------------------------

func (s *Service) nextHitID() string {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	s.hitSeq++
	return fmt.Sprintf("HIT-%04d", s.hitSeq)
}

func (s *Service) nextCaseID() string {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	s.caseSeq++
	return fmt.Sprintf("CASE-%04d", s.caseSeq)
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func cloneEntries(in []domain.ListEntry) []domain.ListEntry {
	out := make([]domain.ListEntry, len(in))
	copy(out, in)
	return out
}

func hitSortKey(h HitInput) string {
	return fmt.Sprintf("%s|%s|%d|%s|%s|%s",
		h.CustomerID, h.ListID, h.Version, h.EntryID, h.MatchedName, h.SubjectKey)
}

func appendUnique(xs []string, x string) []string {
	for _, v := range xs {
		if v == x {
			return xs
		}
	}
	return append(xs, x)
}

// sortedUnique 排序去重。
func sortedUnique(xs []string) []string {
	if len(xs) == 0 {
		return xs
	}
	sort.Strings(xs)
	out := xs[:1]
	for _, x := range xs[1:] {
		if x != out[len(out)-1] {
			out = append(out, x)
		}
	}
	return out
}
