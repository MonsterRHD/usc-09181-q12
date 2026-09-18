package match

import (
	"testing"

	"example.com/09181/q012/internal/domain"
)

func TestNormalizeName(t *testing.T) {
	cases := map[string]string{
		"  Ivan   Petrov ": "ivan petrov",
		"IVAN PETROV":      "ivan petrov",
		"Al-Asad, Omar":    "al asad omar",
		"O'Brien":          "o brien",
	}
	for in, want := range cases {
		if got := NormalizeName(in); got != want {
			t.Errorf("NormalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJaroWinklerKnownValues(t *testing.T) {
	if got := JaroWinkler("martha", "marhta"); got < 0.96 || got > 0.97 {
		t.Errorf("jaro-winkler martha/marhta = %.3f, want ~0.961", got)
	}
	if JaroWinkler("john", "john") != 1 {
		t.Error("identical strings should be 1")
	}
	if JaroWinkler("", "x") != 0 || JaroWinkler("x", "") != 0 {
		t.Error("empty string similarity must be 0")
	}
}

func TestScreenExactAliasAndTokenset(t *testing.T) {
	entries := []domain.ListEntry{{
		EntryID: "E1", PrimaryName: "IVAN PETROV",
		Aliases: []string{"Ivan Petrov Jr"},
	}}
	m := New(0.85)

	// 别名完全一致
	cand := Candidate{Names: []string{"Ivan Petrov Jr"}, HasAddress: false}
	res := m.Screen(cand, entries)
	if len(res) != 1 || !res[0].Match {
		t.Fatalf("exact alias should match, got %+v", res)
	}
	if !hasRule(res[0].Reasons, domain.RuleExactName) {
		t.Error("exact name rule missing")
	}
	if !hasRule(res[0].Reasons, domain.RuleAddressMissing) {
		t.Error("missing address should be annotated")
	}
	if !hasRule(res[0].Reasons, domain.RuleWeakDemographic) {
		t.Error("name-only evidence should be annotated weak")
	}

	// 词序不同的 token-set 命中
	cand2 := Candidate{Names: []string{"Petrov Ivan"}}
	res2 := m.Screen(cand2, entries)
	if len(res2) != 1 || !res2[0].Match {
		t.Fatalf("token-set name should match, got %+v", res2)
	}
}

func TestScreenDOBVetoAndCorroboration(t *testing.T) {
	entries := []domain.ListEntry{{
		EntryID: "E1", PrimaryName: "John Smith", DateOfBirth: "1980-01-01",
	}}
	m := New(0.85)

	// 同名但生日冲突：强否决
	vetoed := m.Screen(Candidate{
		Names: []string{"John Smith"}, DateOfBirth: "1990-12-31", HasAddress: true,
	}, entries)
	if len(vetoed) != 1 || vetoed[0].Match || !vetoed[0].Vetoed {
		t.Fatalf("conflicting DOB must veto the match, got %+v", vetoed)
	}
	if !hasRule(vetoed[0].Reasons, domain.RuleDOBVeto) {
		t.Error("veto rule missing")
	}

	// 同名同生日：命中且 DOB 佐证加分
	confirmed := m.Screen(Candidate{
		Names: []string{"John Smith"}, DateOfBirth: "1980-01-01",
		Nationality: "RU",
	}, entries)
	if len(confirmed) != 1 || !confirmed[0].Match {
		t.Fatalf("equal DOB should confirm, got %+v", confirmed)
	}
	if !hasRule(confirmed[0].Reasons, domain.RuleDOB) {
		t.Error("DOB corroboration missing")
	}
	if confirmed[0].Score <= 0.9 {
		t.Errorf("expected corroborated score above exact name weight, got %.2f", confirmed[0].Score)
	}
}

func TestScreenBelowThreshold(t *testing.T) {
	entries := []domain.ListEntry{{EntryID: "E1", PrimaryName: "Alexander Popov"}}
	m := New(0.95) // 高阈值
	res := m.Screen(Candidate{Names: []string{"Alex Popov"}}, entries)
	for _, r := range res {
		if r.Match {
			t.Fatalf("similarity below threshold must not match: %+v", r)
		}
	}
}

func TestFromCustomer(t *testing.T) {
	c := domain.Customer{
		CustomerID: "C1", LegalName: "Anna Lee", Aliases: []string{"A. Lee"},
		Addresses: []domain.Address{{Country: "SG"}},
	}
	cand := FromCustomer(c)
	if len(cand.Names) != 2 || cand.Names[0] != "Anna Lee" || !cand.HasAddress || cand.CustomerID != "C1" {
		t.Fatalf("unexpected candidate: %+v", cand)
	}
}

func hasRule(reasons []domain.MatchReason, rule string) bool {
	for _, r := range reasons {
		if r.Rule == rule {
			return true
		}
	}
	return false
}
