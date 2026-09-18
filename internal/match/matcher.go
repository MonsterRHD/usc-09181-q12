// Package match 实现与存储无关、可解释的姓名/身份匹配规则。
//
// 每条命中都必须能回答「为什么命中」：匹配器返回触发的规则、明细与权重，
// 案件台据此向合规官展示证据链。地址从不参与同一性判断（跨境客户地址普遍缺失，
// 以地址合并或拆分案件都会出错）。
package match

import (
	"sort"
	"strings"
	"unicode"

	"example.com/09181/q012/internal/domain"
)

// DefaultThreshold 模糊姓名默认命中阈值（Jaro–Winkler 相似度）。
const DefaultThreshold = 0.85

// Candidate 一次待筛查输入：客户主数据可选（临柜/预开户场景可能尚无客户号）。
type Candidate struct {
	CustomerID string
	// Names 为参与匹配的全部原始姓名（法定名 + 别名）。
	Names []string
	DateOfBirth string
	Nationality string
	IDDocuments []string
	HasAddress  bool
}

// FromCustomer 从客户主数据构造筛查候选。
func FromCustomer(c domain.Customer) Candidate {
	names := make([]string, 0, len(c.Aliases)+1)
	if c.LegalName != "" {
		names = append(names, c.LegalName)
	}
	names = append(names, c.Aliases...)
	return Candidate{
		CustomerID:  c.CustomerID,
		Names:       names,
		DateOfBirth: c.DateOfBirth,
		Nationality: c.Nationality,
		IDDocuments: c.IDDocuments,
		HasAddress:  len(c.Addresses) > 0,
	}
}

// Result 单个名单条目的匹配结果。
type Result struct {
	Entry       domain.ListEntry
	MatchedName string // 客户侧触发匹配的原始姓名
	EntryName   string // 名单侧触发匹配的姓名（主名或别名）
	Score       float64
	Reasons     []domain.MatchReason
	Match       bool
	Vetoed      bool // 被强规则否决（如出生日期冲突）
}

// Matcher 线程安全：配置只读，无内部状态。
type Matcher struct {
	threshold float64
}

// New 构造匹配器；threshold<=0 时使用默认阈值。
func New(threshold float64) *Matcher {
	if threshold <= 0 {
		threshold = DefaultThreshold
	}
	return &Matcher{threshold: threshold}
}

// Screen 用候选的全部姓名比对名单条目，返回全部命中结果（调用方负责排序/聚合）。
func (m *Matcher) Screen(cand Candidate, entries []domain.ListEntry) []Result {
	var out []Result
	for _, e := range entries {
		if r, ok := m.matchEntry(cand, e); ok {
			out = append(out, r)
		}
	}
	return out
}

func (m *Matcher) matchEntry(cand Candidate, e domain.ListEntry) (Result, bool) {
	best := nameComparison{}
	for _, cn := range cand.Names {
		for _, en := range append(append([]string{}, e.Aliases...), e.PrimaryName) {
			cmp := compareNames(cn, en)
			if cmp.similarity > best.similarity {
				cmp.candName, cmp.entryName = cn, en
				best = cmp
			}
		}
	}
	if best.candName == "" {
		return Result{}, false
	}

	r := Result{Entry: e, MatchedName: best.candName, EntryName: best.entryName}
	nameWeight := 0.0
	switch {
	case best.exact:
		nameWeight = 0.9
		r.Reasons = append(r.Reasons, domain.MatchReason{
			Rule: domain.RuleExactName, Weight: nameWeight,
			Detail: "name normalized-equal to " + best.entryName,
		})
	case best.similarity >= m.threshold:
		nameWeight = round2(best.similarity * 0.9)
		r.Reasons = append(r.Reasons, domain.MatchReason{
			Rule: domain.RuleFuzzyName, Weight: nameWeight,
			Detail: describeName(best),
		})
	default:
		return Result{}, false
	}
	r.Score = nameWeight

	// 强佐证 / 强否决：出生日期。
	if cand.DateOfBirth != "" && e.DateOfBirth != "" {
		switch {
		case cand.DateOfBirth == e.DateOfBirth:
			r.Score = round2(min1(r.Score + 0.15))
			r.Reasons = append(r.Reasons, domain.MatchReason{Rule: domain.RuleDOB, Detail: "date of birth equal", Weight: 0.15})
		default:
			// 同名不同生日：不是同一人，即使姓名完全一致也否决。
			r.Score = 0
			r.Reasons = append(r.Reasons, domain.MatchReason{
				Rule: domain.RuleDOBVeto, Weight: 0,
				Detail: "date of birth conflicts: " + cand.DateOfBirth + " vs " + e.DateOfBirth,
			})
			r.Vetoed = true
			return r, true
		}
	}

	if cand.Nationality != "" && e.Nationality != "" && strings.EqualFold(cand.Nationality, e.Nationality) {
		r.Score = round2(min1(r.Score + 0.05))
		r.Reasons = append(r.Reasons, domain.MatchReason{Rule: domain.RuleNationality, Detail: "nationality equal", Weight: 0.05})
	}
	if overlapIDs(cand.IDDocuments, e.IDDocuments) {
		r.Score = round2(min1(r.Score + 0.2))
		r.Reasons = append(r.Reasons, domain.MatchReason{Rule: domain.RuleIDDocument, Detail: "identity document overlaps", Weight: 0.2})
	}

	// 标注型规则：解释证据强弱，不改变分数。
	hasStrong := reasonPresent(r.Reasons, domain.RuleDOB) || reasonPresent(r.Reasons, domain.RuleIDDocument)
	if !hasStrong {
		r.Reasons = append(r.Reasons, domain.MatchReason{
			Rule: domain.RuleWeakDemographic, Weight: 0,
			Detail: "only name evidence; no DOB or document corroboration",
		})
	}
	if !cand.HasAddress {
		r.Reasons = append(r.Reasons, domain.MatchReason{
			Rule: domain.RuleAddressMissing, Weight: 0,
			Detail: "customer has no address on file; address cannot confirm identity",
		})
	}

	// 是否命中取决于姓名门槛与强否决；证据总分用于向合规官解释强弱，
	// 不因缺少佐证而把已过门槛的姓名重新压到门槛下。
	r.Match = true
	return r, true
}

// ---------------------------------------------------------------------------
// 姓名规范化与相似度
// ---------------------------------------------------------------------------

type nameComparison struct {
	candName   string
	entryName  string
	similarity float64
	exact      bool
	tokenSet   bool
}

func describeName(c nameComparison) string {
	s := "jaro-winkler similarity " + floatText(c.similarity)
	if c.tokenSet {
		s += " on token-set (name word order differs)"
	}
	return s
}

func compareNames(a, b string) nameComparison {
	na, nb := NormalizeName(a), NormalizeName(b)
	if na == "" || nb == "" {
		return nameComparison{candName: a, entryName: b}
	}
	direct := JaroWinkler(na, nb)
	ta, tb := tokenSetForm(na), tokenSetForm(nb)
	setSim := 0.0
	if ta != na || tb != nb {
		setSim = JaroWinkler(ta, tb)
	}
	cmp := nameComparison{
		candName:   a,
		entryName:  b,
		similarity: maxFloat(direct, setSim),
		exact:      na == nb || ta == tb,
		tokenSet:   setSim > direct,
	}
	return cmp
}

// NormalizeName 大小写折叠、去除标点、合并空白。不做激进的音译/缩写展开，
// 避免把解释不了的变换混入规则。
func NormalizeName(s string) string {
	var b strings.Builder
	prevSpace := true
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			prevSpace = false
		case unicode.IsSpace(r) || r == '-' || r == '\'' || r == '.' || r == ',':
			if !prevSpace {
				b.WriteRune(' ')
				prevSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}

func tokenSetForm(norm string) string {
	tokens := strings.Fields(norm)
	sort.Strings(tokens)
	return strings.Join(tokens, " ")
}

// JaroWinkler 返回 [0,1] 相似度，前缀加成上限 scaling*maxPrefix(4)。
func JaroWinkler(a, b string) float64 {
	if a == b {
		return 1
	}
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 || lb == 0 {
		return 0
	}
	matchDist := maxInt(la, lb)/2 - 1
	if matchDist < 0 {
		matchDist = 0
	}
	aMatch := make([]bool, la)
	bMatch := make([]bool, lb)
	matches := 0
	for i := 0; i < la; i++ {
		start := maxInt(0, i-matchDist)
		end := minInt(i+matchDist+1, lb)
		for j := start; j < end; j++ {
			if bMatch[j] || ra[i] != rb[j] {
				continue
			}
			aMatch[i] = true
			bMatch[j] = true
			matches++
			break
		}
	}
	if matches == 0 {
		return 0
	}
	// 换位
	transpositions := 0
	k := 0
	for i := 0; i < la; i++ {
		if !aMatch[i] {
			continue
		}
		for !bMatch[k] {
			k++
		}
		if ra[i] != rb[k] {
			transpositions++
		}
		k++
	}
	jaro := (float64(matches)/float64(la) +
		float64(matches)/float64(lb) +
		float64(matches-transpositions/2)/float64(matches)) / 3

	prefix := 0
	for prefix < 4 && prefix < la && prefix < lb && ra[prefix] == rb[prefix] {
		prefix++
	}
	const scaling = 0.1
	return jaro + float64(prefix)*scaling*(1-jaro)
}

// NormalizeID 证件号比较前的规范化：去分隔符、大写。
func NormalizeID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func overlapIDs(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, x := range a {
		if n := NormalizeID(x); n != "" {
			set[n] = struct{}{}
		}
	}
	for _, y := range b {
		if _, ok := set[NormalizeID(y)]; ok {
			return true
		}
	}
	return false
}

func reasonPresent(rs []domain.MatchReason, rule string) bool {
	for _, r := range rs {
		if r.Rule == rule {
			return true
		}
	}
	return false
}

func min1(x float64) float64 {
	if x > 1 {
		return 1
	}
	return x
}

func round2(x float64) float64 { return float64(int(x*100+0.5)) / 100 }

func floatText(x float64) string {
	// 避免引入 strconv 造成的格式差异；两位小数足够解释用途。
	if x >= 1 {
		return "1.00"
	}
	return "0." + twoDigits(x)
}

func twoDigits(x float64) string {
	n := int(x*100 + 0.5)
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}

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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
