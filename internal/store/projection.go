package store

import (
	"sort"
	"strings"
	"sync"
	"time"

	"example.com/09181/q012/internal/domain"
)

// ListVersionState 名单的一个已发布版本。撤回与重新发布都不动旧版本数据。
type ListVersionState struct {
	Version     int
	BaseVersion int // 重新发布时所基于的撤回版本，否则为 0
	Entries     []domain.ListEntry
	Status      domain.ListStatus
	PublishedAt time.Time
	WithdrawnAt time.Time
}

// ListState 一份名单的全部版本谱系。
type ListState struct {
	ListID   string
	Versions map[int]*ListVersionState
}

// ListVersions 返回版本映射（只读使用）。
func (l *ListState) ListVersions() map[int]*ListVersionState { return l.Versions }

// EntryByID 按条目标识查找版本内的名单条目。
func (v *ListVersionState) EntryByID(id string) (domain.ListEntry, bool) {
	for _, e := range v.Entries {
		if e.EntryID == id {
			return e, true
		}
	}
	return domain.ListEntry{}, false
}

// ActiveVersion 返回当前有效版本号；不存在有效版本时返回 0。
func (l *ListState) ActiveVersion() int {
	var best int
	for n, v := range l.Versions {
		if v.Status == domain.ListActive && n > best {
			best = n
		}
	}
	return best
}

// CaseState 案件聚合的投影状态。HitIDs 永远只追加，合并也不搬移命中。
type CaseState struct {
	CaseID          string
	CustomerID      string
	Status          domain.CaseStatus
	AggregationKey  string
	AggregationRule string
	Timezone        string
	HitIDs          []string
	OpenedAt        time.Time
	Retroactive     bool

	AssignedOfficer string

	// SLA 时间轴：AnchorAt 为当前提醒纪元起点；暂停期间累计 PausedTotal。
	AnchorAt    time.Time
	Epoch       int
	Paused      bool
	PauseReason string
	PausedTotal time.Duration
	EscalatedAt time.Time

	// 合并关系构成一棵以幸存案件为根的树，AbsorbedInto 指向直接父节点。
	AbsorbedInto string
	Absorbed     []string

	Notes        []domain.Note
	Dispositions []domain.Disposition // 全历史保留；CurrentDisposition 取最后一条
}

// EffectiveStatus 把暂停标记折算为显式状态：暂停中的案件对外显示 paused。
func (c *CaseState) EffectiveStatus() domain.CaseStatus {
	if c.Paused {
		return domain.StatusPaused
	}
	return c.Status
}

// CurrentDisposition 返回最近一次处置；案件重开后可能为空。
func (c *CaseState) CurrentDisposition() *domain.Disposition {
	if len(c.Dispositions) == 0 {
		return nil
	}
	d := c.Dispositions[len(c.Dispositions)-1]
	return &d
}

// DueMilestone 一个已到期但尚未在本纪元发出的提醒。
type DueMilestone struct {
	CaseID    string
	Epoch     int
	Milestone string
	DueAt     time.Time
}

// Projection 是从事件日志重放得到的全部读模型，并发安全。
type Projection struct {
	mu sync.RWMutex

	customers map[string]domain.Customer
	officers  map[string]domain.Officer
	lists     map[string]*ListState
	hits      map[string]domain.Hit
	hitByKey  map[string]domain.Hit   // 业务键 -> 首条命中（重复命中识别）
	caseByKey map[string]string       // 聚合键 -> 当前根案件
	cases     map[string]*CaseState

	// firedReminders: caseID|epoch|milestone -> 已记录的 ReminderFired
	firedReminders map[string]domain.ReminderFired

	// pauseStarted: caseID -> 当前暂停周期起点，仅暂停中存在。
	pauseStarted map[string]time.Time
}

// NewProjection 创建空投影。
func NewProjection() *Projection {
	return &Projection{
		customers:      map[string]domain.Customer{},
		officers:       map[string]domain.Officer{},
		lists:          map[string]*ListState{},
		hits:           map[string]domain.Hit{},
		hitByKey:       map[string]domain.Hit{},
		caseByKey:      map[string]string{},
		cases:          map[string]*CaseState{},
		firedReminders: map[string]domain.ReminderFired{},
		pauseStarted:   map[string]time.Time{},
	}
}

// Apply 让投影消费一条领域事件。由 EventStore 订阅回调在加锁外调用。
func (p *Projection) Apply(eventType string, data any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.apply(eventType, data)
}

func (p *Projection) apply(eventType string, data any) {
	switch e := data.(type) {
	case *domain.CustomerUpserted:
		p.customers[e.Customer.CustomerID] = e.Customer

	case *domain.OfficerProvisioned:
		p.officers[e.Officer.OfficerID] = e.Officer

	case *domain.ListPublished:
		l := p.lists[e.ListID]
		if l == nil {
			l = &ListState{ListID: e.ListID, Versions: map[int]*ListVersionState{}}
			p.lists[e.ListID] = l
		}
		l.Versions[e.Version] = &ListVersionState{
			Version: e.Version, Entries: e.Entries,
			Status: domain.ListActive, PublishedAt: e.PublishedAt,
		}

	case *domain.ListWithdrawn:
		if l := p.lists[e.ListID]; l != nil {
			if v := l.Versions[e.Version]; v != nil {
				v.Status = domain.ListWithdrawn
				v.WithdrawnAt = e.WithdrawnAt
			}
		}

	case *domain.ListRepublished:
		l := p.lists[e.ListID]
		if l == nil {
			l = &ListState{ListID: e.ListID, Versions: map[int]*ListVersionState{}}
			p.lists[e.ListID] = l
		}
		l.Versions[e.Version] = &ListVersionState{
			Version: e.Version, BaseVersion: e.BaseVersion, Entries: e.Entries,
			Status: domain.ListActive, PublishedAt: e.At,
		}

	case *domain.HitRecorded:
		p.hits[e.Hit.HitID] = e.Hit
		if _, exists := p.hitByKey[e.Hit.BusinessKey()]; !exists {
			p.hitByKey[e.Hit.BusinessKey()] = e.Hit
		}

	case *domain.CaseOpened:
		p.cases[e.CaseID] = &CaseState{
			CaseID: e.CaseID, CustomerID: e.CustomerID,
			Status: domain.StatusOpen, AggregationKey: e.AggregationKey,
			AggregationRule: e.AggregationRule, Timezone: e.Timezone,
			HitIDs:          append([]string{}, e.HitIDs...),
			OpenedAt:        e.OpenedAt, AnchorAt: e.OpenedAt,
			Epoch: 1, Retroactive: e.Retroactive,
		}
		p.caseByKey[e.AggregationKey] = e.CaseID

	case *domain.HitsLinked:
		if c := p.cases[e.CaseID]; c != nil {
			c.HitIDs = appendMissing(c.HitIDs, e.HitIDs)
		}

	case *domain.CasesMerged:
		if survivor := p.cases[e.SurvivorCaseID]; survivor != nil {
			survivor.Absorbed = appendMissing(survivor.Absorbed, []string{e.AbsorbedCaseID})
		}
		if absorbed := p.cases[e.AbsorbedCaseID]; absorbed != nil {
			absorbed.Status = domain.StatusMergedAway
			absorbed.AbsorbedInto = e.SurvivorCaseID
			// 被吸收案件的聚合键以后解析到幸存案件，新命中不会再开案。
			if absorbed.AggregationKey != "" {
				p.caseByKey[absorbed.AggregationKey] = e.SurvivorCaseID
			}
		}

	case *domain.CaseAssigned:
		if c := p.cases[e.CaseID]; c != nil {
			c.AssignedOfficer = e.OfficerID
		}

	case *domain.CasePaused:
		if c := p.cases[e.CaseID]; c != nil && !c.Paused {
			c.Paused = true
			c.PauseReason = e.Reason
			p.pauseStarted[e.CaseID] = e.At
		}

	case *domain.CaseResumed:
		if c := p.cases[e.CaseID]; c != nil && c.Paused {
			c.Paused = false
			c.PauseReason = ""
			if started, ok := p.pauseStarted[e.CaseID]; ok {
				c.PausedTotal += e.At.Sub(started)
				delete(p.pauseStarted, e.CaseID)
			}
		}

	case *domain.CaseReopened:
		if c := p.cases[e.CaseID]; c != nil {
			c.Status = domain.StatusOpen
			c.AnchorAt = e.At
			c.Epoch = e.Epoch
			c.Paused = false
			c.PauseReason = ""
			c.PausedTotal = 0
			c.EscalatedAt = time.Time{}
			c.HitIDs = appendMissing(c.HitIDs, e.HitIDs)
			// 旧处置已被新证据推翻，不再作为「当前处置」展示；
			// 处置历史仍完整保留在事件日志中，可由审计重放得到。
			c.Dispositions = nil
			delete(p.pauseStarted, e.CaseID)
		}

	case *domain.CaseEscalated:
		if c := p.cases[e.CaseID]; c != nil {
			c.Status = domain.StatusEscalated
			c.EscalatedAt = e.At
		}

	case *domain.ReminderFired:
		p.firedReminders[reminderKey(e.CaseID, e.Epoch, e.Milestone)] = *e

	case *domain.NoteRecorded:
		if c := p.cases[e.Note.CaseID]; c != nil {
			c.Notes = append(c.Notes, e.Note)
		}

	case *domain.DispositionRecorded:
		if c := p.cases[e.Disposition.CaseID]; c != nil {
			c.Status = e.Disposition.Decision.Status()
			c.Dispositions = append(c.Dispositions, e.Disposition)
			c.Paused = false
			delete(p.pauseStarted, e.Disposition.CaseID)
		}
	}
}

func reminderKey(caseID string, epoch int, milestone string) string {
	return caseID + "|" + itoa(epoch) + "|" + milestone
}

func appendMissing(slice []string, add []string) []string {
	seen := make(map[string]struct{}, len(slice))
	for _, s := range slice {
		seen[s] = struct{}{}
	}
	for _, s := range add {
		if _, ok := seen[s]; !ok {
			slice = append(slice, s)
			seen[s] = struct{}{}
		}
	}
	return slice
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

// Snapshot 为一次性读出的不可变案件视图（深拷贝，调用方可安全持有）。
type Snapshot struct {
	Case         CaseState
	Hits         []domain.Hit
	Notes        []domain.Note
	Disposition  *domain.Disposition
	RootCaseID   string
	RelatedCases []string // 合并树中与本案相关的全部案件（含自身）
}

// Officer 返回人员名册条目。
func (p *Projection) Officer(id string) (domain.Officer, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	o, ok := p.officers[id]
	return o, ok
}

// Customer 返回复制后的客户主数据。
func (p *Projection) Customer(id string) (domain.Customer, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	c, ok := p.customers[id]
	return c, ok
}

// Customers 返回全部客户（复制）。
func (p *Projection) Customers() []domain.Customer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]domain.Customer, 0, len(p.customers))
	for _, c := range p.customers {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CustomerID < out[j].CustomerID })
	return out
}

// List 返回名单状态快照。
func (p *Projection) List(id string) (*ListState, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	l, ok := p.lists[id]
	return l, ok
}

// Hit 返回单条命中。
func (p *Projection) Hit(id string) (domain.Hit, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	h, ok := p.hits[id]
	return h, ok
}

// HitByKey 按业务键返回首条命中（重复命中识别用）。
func (p *Projection) HitByKey(key string) (domain.Hit, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	h, ok := p.hitByKey[key]
	return h, ok
}

// CaseByAggregationKey 按聚合键找到当前根案件；案件被合并后解析到幸存案件。
func (p *Projection) CaseByAggregationKey(key string) (string, *CaseState) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	id, ok := p.caseByKey[key]
	if !ok {
		return "", nil
	}
	for i := 0; i < 10000; i++ {
		c := p.cases[id]
		if c == nil {
			return "", nil
		}
		if c.AbsorbedInto == "" {
			return id, cloneCase(c)
		}
		id = c.AbsorbedInto
	}
	return "", nil
}

// AllLists 返回全部名单（按 ListID 排序的状态指针快照，内容只读使用）。
func (p *Projection) AllLists() []*ListState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	ids := make([]string, 0, len(p.lists))
	for id := range p.lists {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*ListState, 0, len(ids))
	for _, id := range ids {
		out = append(out, p.lists[id])
	}
	return out
}

// RootCase 沿合并指针找到幸存（根）案件。
func (p *Projection) RootCase(caseID string) (string, *CaseState, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.rootLocked(caseID)
}

// rootLocked 调用方必须已持有读锁或写锁（避免递归 RLock 在有写等待时死锁）。
func (p *Projection) rootLocked(caseID string) (string, *CaseState, bool) {
	root := caseID
	for i := 0; i < 10000; i++ {
		c, ok := p.cases[root]
		if !ok {
			return "", nil, false
		}
		if c.AbsorbedInto == "" {
			return root, cloneCase(c), true
		}
		root = c.AbsorbedInto
	}
	return "", nil, false
}

// CaseSnapshot 组装案件全貌：自身命中、合并树中被吸收案件的命中（追加关系，只读展开）。
func (p *Projection) CaseSnapshot(caseID string) (Snapshot, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	rootID, root, ok := p.rootLocked(caseID)
	if !ok {
		return Snapshot{}, false
	}
	snap := Snapshot{Case: *root, RootCaseID: rootID}

	related := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		if related[id] {
			return
		}
		related[id] = true
		c := p.cases[id]
		if c == nil {
			return
		}
		for _, child := range c.Absorbed {
			walk(child)
		}
	}
	walk(rootID)
	for id := range related {
		snap.RelatedCases = append(snap.RelatedCases, id)
	}
	sort.Strings(snap.RelatedCases)

	// 命中顺序：先根案件自身（按追加顺序），再各被吸收案件，案件间按 ID 排序保证确定性。
	var others []string
	for _, id := range snap.RelatedCases {
		if id != rootID {
			others = append(others, id)
		}
	}
	sort.Strings(others)
	ordered := append([]string{rootID}, others...)
	for _, id := range ordered {
		c := p.cases[id]
		for _, hid := range c.HitIDs {
			if h, ok := p.hits[hid]; ok {
				snap.Hits = append(snap.Hits, h)
			}
		}
	}
	snap.Notes = append([]domain.Note{}, root.Notes...)
	snap.Disposition = root.CurrentDisposition()
	return snap, true
}

// CaseOwn 返回案件自身的状态副本，不沿合并指针解析（审计核对原命中归属用）。
func (p *Projection) CaseOwn(caseID string) (*CaseState, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	c, ok := p.cases[caseID]
	if !ok {
		return nil, false
	}
	return cloneCase(c), true
}

// Cases 按状态过滤；空 statuses 返回全部根案件（含已终态）。
func (p *Projection) Cases(statuses ...domain.CaseStatus) []CaseState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	want := map[domain.CaseStatus]bool{}
	for _, s := range statuses {
		want[s] = true
	}
	out := make([]CaseState, 0, len(p.cases))
	for _, c := range p.cases {
		if c.AbsorbedInto != "" {
			continue // 被吸收案件通过根案件访问
		}
		if len(want) == 0 || want[c.EffectiveStatus()] {
			out = append(out, *cloneCase(c))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CaseID < out[j].CaseID })
	return out
}

// AssignedCases 返回分配给某合规官的根案件。
func (p *Projection) AssignedCases(officerID string) []CaseState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := []CaseState{}
	for _, c := range p.cases {
		if c.AbsorbedInto == "" && c.AssignedOfficer == officerID {
			out = append(out, *cloneCase(c))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CaseID < out[j].CaseID })
	return out
}

// DueReminders 扫描全部案件，返回 now 之前到期、未暂停、未终态且本纪元尚未发出的提醒。
// 结果按 DueAt 升序（同刻按 CaseID、里程碑），满足「恢复后按发生顺序推动提醒」。
func (p *Projection) DueReminders(now time.Time, warningWithin, sla time.Duration) []DueMilestone {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var out []DueMilestone
	for _, c0 := range p.cases {
		c := c0
		if c.AbsorbedInto != "" || c.Status.Terminal() || c.Paused {
			continue
		}
		warningAt := c.AnchorAt.Add(c.PausedTotal + sla - warningWithin)
		escalationAt := c.AnchorAt.Add(c.PausedTotal + sla)

		warnKey := reminderKey(c.CaseID, c.Epoch, MilestoneWarning)
		escKey := reminderKey(c.CaseID, c.Epoch, MilestoneEscalation)
		_, warnFired := p.firedReminders[warnKey]
		_, escFired := p.firedReminders[escKey]

		if !warnFired && !warningAt.After(now) {
			out = append(out, DueMilestone{CaseID: c.CaseID, Epoch: c.Epoch, Milestone: MilestoneWarning, DueAt: warningAt})
		}
		if !escFired && !escalationAt.After(now) {
			out = append(out, DueMilestone{CaseID: c.CaseID, Epoch: c.Epoch, Milestone: MilestoneEscalation, DueAt: escalationAt})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].DueAt.Equal(out[j].DueAt) {
			return out[i].DueAt.Before(out[j].DueAt)
		}
		if out[i].CaseID != out[j].CaseID {
			return out[i].CaseID < out[j].CaseID
		}
		return out[i].Milestone < out[j].Milestone
	})
	return out
}

// ReminderFired 判断某纪元某里程碑是否已发出（幂等检查）。
func (p *Projection) ReminderFired(caseID string, epoch int, milestone string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, ok := p.firedReminders[reminderKey(caseID, epoch, milestone)]
	return ok
}

// ActiveListEntries 返回某名单现行版本的条目；无有效版本时第二返回值为 false。
func (p *Projection) ActiveListEntries(listID string) ([]domain.ListEntry, int, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	l, ok := p.lists[listID]
	if !ok {
		return nil, 0, false
	}
	v := l.ActiveVersion()
	if v == 0 {
		return nil, 0, false
	}
	return append([]domain.ListEntry{}, l.Versions[v].Entries...), v, true
}

// VersionEntries 返回指定版本条目（即使已撤回，用于审计与回溯基准）。
func (p *Projection) VersionEntries(listID string, version int) (*ListVersionState, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	l, ok := p.lists[listID]
	if !ok {
		return nil, false
	}
	v, ok := l.Versions[version]
	return v, ok
}

func cloneCase(c *CaseState) *CaseState {
	cp := *c
	cp.HitIDs = append([]string{}, c.HitIDs...)
	cp.Absorbed = append([]string{}, c.Absorbed...)
	cp.Notes = append([]domain.Note{}, c.Notes...)
	cp.Dispositions = append([]domain.Disposition{}, c.Dispositions...)
	return &cp
}

// 提醒里程碑常量。
const (
	MilestoneWarning    = "warning"
	MilestoneEscalation = "escalation"
)

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b strings.Builder
	for n > 0 {
		b.WriteByte(byte('0' + n%10))
		n /= 10
	}
	r := []byte(b.String())
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}
