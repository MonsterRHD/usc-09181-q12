package screening

import (
	"strings"
	"unicode"
)

// NormalizeName 把姓名折叠成可比较的形式：小写、去除标点/符号、合并空白。
// 例："V.  Petrov" -> "v petrov"。不做跨语言转写，别名关系由客户主数据/名单条目显式提供。
func NormalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastSpace := true
	for _, r := range s {
		switch {
		case unicode.IsPunct(r), unicode.IsSymbol(r), unicode.IsSpace(r):
			if !lastSpace {
				b.WriteRune(' ')
			}
			lastSpace = true
		default:
			b.WriteRune(r)
			lastSpace = false
		}
	}
	return strings.TrimSpace(b.String())
}

// NormalizeAddress 地址归一化，缺失地址（空串）归一化后仍为空串。
func NormalizeAddress(s string) string { return NormalizeName(s) }

// NormalizeNationalID 证件号归一化：大写、只保留字母数字。
func NormalizeNationalID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(s)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func NameEqual(a, b string) bool {
	na, nb := NormalizeName(a), NormalizeName(b)
	return na != "" && na == nb
}

// MaskName 按字符掩码原始姓名，保留每个 token 的首字符。
// "John Smith" -> "J*** S****"；"张伟" -> "张*"。空串保持空串（地址缺失不泄露姓名）。
func MaskName(s string) string {
	if s == "" {
		return ""
	}
	tokens := strings.Fields(s)
	if len(tokens) == 0 {
		return strings.Repeat("*", len([]rune(s)))
	}
	masked := make([]string, len(tokens))
	for i, tok := range tokens {
		r := []rune(tok)
		if len(r) <= 1 {
			masked[i] = string(r)
			continue
		}
		masked[i] = string(r[0]) + strings.Repeat("*", len(r)-1)
	}
	return strings.Join(masked, " ")
}
