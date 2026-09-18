package screening

import (
	"fmt"
	"sort"
	"strings"
)

// MatchEdge 描述两条命中被归入同一案件的依据（可解释性）。
type MatchEdge struct {
	A, B  string
	Rule  string
	Detail string
}

// AggregationPlan 聚合方案：命中分组 + 每条归并边的规则说明 + 因证据不足而未合并的相邻命中。
type AggregationPlan struct {
	Groups      [][]string
	Edges       []MatchEdge
	KeptApart   []MatchEdge // 看似相似但依据规则被案件边界隔开的命中对
}

// Matcher 按可解释的确定性规则聚合命中。它不做模糊黑箱评分：
// 每条合并边都带规则名与细节，复核时可逐条审计。
type Matcher struct {
	customers map[string]Customer
	entries   map[string]ListEntry
}

func NewMatcher(customers []Customer, entries []ListEntry) *Matcher {
	m := &Matcher{customers: map[string]Customer{}, entries: map[string]ListEntry{}}
	for _, c := range customers {
		m.customers[c.ID] = c
	}
	for _, e := range entries {
		m.entries[e.ID] = e
	}
	return m
}

// aliasNames 返回某客户关联的全部姓名形式（法定名+主数据别名）。
func (m *Matcher) customerNameSet(customerID string) map[string]bool {
	set := map[string]bool{}
	c, ok := m.customers[customerID]
	if !ok {
		return set
	}
	for _, n := range append([]string{c.LegalName}, c.Aliases...) {
		if k := NormalizeName(n); k != "" {
			set[k] = true
		}
	}
	return set
}

// Plan 对一批命中计算聚合方案。
//
// 案件边界（硬性规则，优先级最高）：客户主数据 ID 不同的命中永不合并，
// 即使姓名高度相似、地址缺失也一样——这正是手工并案最容易放错材料的情形。
//
// 可合并规则（满足其一，且不违反硬边界）：
//  1. SAME_CUSTOMER_ID       ：两条命中指向同一客户主数据记录；
//  2. NATIONAL_ID_EXACT      ：归一化证件号相同且非空；
//  3. CUSTOMER_ALIAS_CHAIN   ：两个命中姓名都出现在同一客户的法定名/别名集合中
//                              （同一人多别名同时命中）；
//  4. NAME_ADDRESS_EXACT     ：归一化姓名相同，且归一化地址相同且非空。
//
// 地址缺失时规则 4 不生效；单纯"姓名相似"不足以跨客户并案。
func (m *Matcher) Plan(hits []Hit) AggregationPlan {
	n := len(hits)
	uf := make([]int, n)
	for i := range uf {
		uf[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for uf[x] != x {
			uf[x] = uf[uf[x]]
			x = uf[x]
		}
		return x
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			uf[rb] = ra
		}
	}

	var edges, keptApart []MatchEdge

	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			a, b := hits[i], hits[j]

			// 硬边界：不同客户主数据 ID。
			if a.CustomerID != "" && b.CustomerID != "" && a.CustomerID != b.CustomerID {
				if namesLookSimilar(a.RawName, b.RawName) {
					keptApart = append(keptApart, MatchEdge{
						A: a.ID, B: b.ID, Rule: "BOUNDARY_DIFFERENT_CUSTOMER",
						Detail: fmt.Sprintf("customer %q vs %q：姓名相似但主数据指向不同客户，禁止并案", a.CustomerID, b.CustomerID),
					})
				}
				continue
			}

			// 规则按"最能解释为何合并"的顺序判定。
			sameCustomer := a.CustomerID != "" && a.CustomerID == b.CustomerID
			switch {
			case sameCustomer && m.aliasHit(a, b, m.customerNameSet(a.CustomerID)):
				union(i, j)
				edges = append(edges, MatchEdge{A: a.ID, B: b.ID, Rule: "CUSTOMER_ALIAS_CHAIN",
					Detail: fmt.Sprintf("同一客户 %s 以别名 %q 与 %q 同时命中", a.CustomerID, a.RawName, b.RawName)})
			case sameCustomer:
				union(i, j)
				edges = append(edges, MatchEdge{A: a.ID, B: b.ID, Rule: "SAME_CUSTOMER_ID",
					Detail: fmt.Sprintf("两条命中均指向客户 %s", a.CustomerID)})
			case NormalizeNationalID(a.NationalID) != "" &&
				NormalizeNationalID(a.NationalID) == NormalizeNationalID(b.NationalID):
				union(i, j)
				edges = append(edges, MatchEdge{A: a.ID, B: b.ID, Rule: "NATIONAL_ID_EXACT",
					Detail: fmt.Sprintf("归一化证件号一致：%s", NormalizeNationalID(a.NationalID))})
			case linkedByListEntry(a, b, m.entries):
				union(i, j)
				edges = append(edges, MatchEdge{A: a.ID, B: b.ID, Rule: "LIST_ALIAS_CHAIN",
					Detail: fmt.Sprintf("姓名 %q 与 %q 经同一名单条目 %s 的别名集合连通", a.RawName, b.RawName, a.ListEntryID)})
			default:
				na := NormalizeAddress(a.Address)
				if na != "" && na == NormalizeAddress(b.Address) && NameEqual(a.RawName, b.RawName) {
					union(i, j)
					edges = append(edges, MatchEdge{A: a.ID, B: b.ID, Rule: "NAME_ADDRESS_EXACT",
						Detail: fmt.Sprintf("归一化姓名与地址均一致（%s）", na)})
				}
			}
		}
	}

	groupsByRoot := map[int][]string{}
	for i, h := range hits {
		r := find(i)
		groupsByRoot[r] = append(groupsByRoot[r], h.ID)
	}
	var groups [][]string
	for _, g := range groupsByRoot {
		sort.Strings(g)
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i][0] < groups[j][0] })
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].A != edges[j].A {
			return edges[i].A < edges[j].A
		}
		return edges[i].B < edges[j].B
	})
	sort.Slice(keptApart, func(i, j int) bool {
		if keptApart[i].A != keptApart[j].A {
			return keptApart[i].A < keptApart[j].A
		}
		return keptApart[i].B < keptApart[j].B
	})
	return AggregationPlan{Groups: groups, Edges: edges, KeptApart: keptApart}
}

// aliasHit 判断两条同客户命中是否以客户主数据中的不同姓名形式（法定名/别名）出现，
// 即"同一人多别名同时命中"的情形。
func (m *Matcher) aliasHit(a, b Hit, set map[string]bool) bool {
	na, nb := NormalizeName(a.RawName), NormalizeName(b.RawName)
	if na == "" || nb == "" || na == nb {
		return false
	}
	return set[na] && set[nb]
}

// linkedByListEntry 判断两个无共同客户 ID 的命中是否经同一名单条目的姓名/别名集合连通。
func (m *Matcher) linkedByListEntry(a, b Hit) bool {
	if a.ListEntryID == "" || a.ListEntryID != b.ListEntryID {
		return false
	}
	e, ok := m.entries[a.ListEntryID]
	if !ok {
		return false
	}
	set := map[string]bool{}
	for _, n := range append([]string{e.Name}, e.Aliases...) {
		if k := NormalizeName(n); k != "" {
			set[k] = true
		}
	}
	na, nb := NormalizeName(a.RawName), NormalizeName(b.RawName)
	// 同名命中属于 NAME_ADDRESS_EXACT 的范畴；别名链要求两个不同的姓名形式。
	return na != "" && nb != "" && na != nb && set[na] && set[nb]
}

// namesLookSimilar 启发式：用于在 KeptApart 报告中提示"险些错并"的命中对。
// 归一化后首 token 相同，或一个包含另一个。
func namesLookSimilar(a, b string) bool {
	na, nb := NormalizeName(a), NormalizeName(b)
	if na == "" || nb == "" {
		return false
	}
	if na == nb {
		return true
	}
	tA, tB := strings.Fields(na), strings.Fields(nb)
	if len(tA) > 0 && len(tB) > 0 && tA[0] == tB[0] {
		return true
	}
	return strings.Contains(na, nb) || strings.Contains(nb, na)
}
