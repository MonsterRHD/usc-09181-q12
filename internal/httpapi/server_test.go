package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"example.com/09181/q012/internal/service"
	"example.com/09181/q012/internal/store"
)

func bootServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	es, err := store.NewEventStore(path)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := service.New(es, service.Config{
		SLA: 24 * time.Hour, WarningLead: 4 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(NewServer(svc))
	return ts, ts.Close
}

func do(t *testing.T, ts *httptest.Server, method, path, actor string, body any) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if actor != "" {
		req.Header.Set("X-Officer-ID", actor)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestFullWorkflowOverHTTP(t *testing.T) {
	ts, closeFn := bootServer(t)
	defer closeFn()

	// 健康检查
	if code, body := do(t, ts, "GET", "/health", "", nil); code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("health = %d %v", code, body)
	}

	// 名册
	for _, o := range []map[string]string{
		{"officer_id": "agent1", "name": "A", "role": "agent"},
		{"officer_id": "off1", "name": "O", "role": "officer"},
		{"officer_id": "rev1", "name": "R", "role": "reviewer"},
	} {
		if code, body := do(t, ts, "POST", "/v1/admin/officers", "", o); code != http.StatusCreated {
			t.Fatalf("provision %s: %d %v", o["officer_id"], code, body)
		}
	}

	// 未鉴权被拒
	if code, _ := do(t, ts, "GET", "/v1/cases", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d, want 401", code)
	}

	// 客服不能维护客户主数据
	if code, _ := do(t, ts, "PUT", "/v1/customers", "agent1", map[string]any{
		"customer_id": "C1", "legal_name": "John Smith",
	}); code != http.StatusForbidden {
		t.Fatalf("agent upsert customer = %d, want 403", code)
	}

	// 合规官维护主数据与名单
	if code, body := do(t, ts, "PUT", "/v1/customers", "off1", map[string]any{
		"customer_id": "C1", "legal_name": "John Smith", "timezone": "Asia/Singapore",
	}); code != http.StatusOK {
		t.Fatalf("upsert customer: %d %v", code, body)
	}
	if code, body := do(t, ts, "POST", "/v1/lists/OFAC/publish", "off1", map[string]any{
		"version": 1,
		"entries": []map[string]any{{
			"entry_id": "E1", "primary_name": "John Smith", "date_of_birth": "1980-01-01",
		}},
	}); code != http.StatusCreated {
		t.Fatalf("publish list: %d %v", code, body)
	}

	// 实时筛查开案
	if code, body := do(t, ts, "POST", "/v1/screen", "off1", map[string]any{
		"customer_ids": []string{"C1"},
	}); code != http.StatusOK {
		t.Fatalf("screen: %d %v", code, body)
	}

	// 客服看案件：姓名被遮蔽
	code, listBody := do(t, ts, "GET", "/v1/cases", "agent1", nil)
	if code != http.StatusOK {
		t.Fatalf("list cases: %d", code)
	}
	cases := listBody["cases"].([]any)
	if len(cases) != 1 {
		t.Fatalf("want 1 case, got %v", cases)
	}
	caseMap := cases[0].(map[string]any)
	hits := caseMap["hits"].([]any)
	if hits[0].(map[string]any)["matched_name"] != "***REDACTED***" {
		t.Fatalf("agent must see redacted name: %v", hits[0])
	}

	// 合规官能看到原始姓名
	_, officerBody := do(t, ts, "GET", "/v1/cases", "off1", nil)
	officerCase := officerBody["cases"].([]any)[0].(map[string]any)
	caseID := officerCase["case_id"].(string)
	if officerCase["customer"].(map[string]any)["legal_name"] != "John Smith" {
		t.Fatal("officer must see original legal name")
	}

	// 客服不能解除
	if code, _ := do(t, ts, "POST", "/v1/cases/"+caseID+"/resolve", "agent1", map[string]any{
		"decision": "false_positive", "evidence_summary": "x",
	}); code != http.StatusForbidden {
		t.Fatalf("agent resolve = %d, want 403", code)
	}
	// 合规官也不能解除（仅复核人）
	if code, _ := do(t, ts, "POST", "/v1/cases/"+caseID+"/resolve", "off1", map[string]any{
		"decision": "false_positive", "evidence_summary": "x",
	}); code != http.StatusForbidden {
		t.Fatalf("officer resolve = %d, want 403", code)
	}
	// 解除必须带证据
	if code, _ := do(t, ts, "POST", "/v1/cases/"+caseID+"/resolve", "rev1", map[string]any{
		"decision": "false_positive",
	}); code != http.StatusConflict {
		t.Fatalf("missing evidence = %d, want 409", code)
	}
	// 合法解除
	if code, body := do(t, ts, "POST", "/v1/cases/"+caseID+"/resolve", "rev1", map[string]any{
		"decision": "false_positive", "evidence_summary": "DOB mismatch, documents reviewed",
	}); code != http.StatusOK {
		t.Fatalf("resolve: %d %v", code, body)
	}

	// 撤回 + 重新发布触发回溯重开
	if code, body := do(t, ts, "POST", "/v1/lists/OFAC/withdraw?version=1", "off1", nil); code != http.StatusOK {
		t.Fatalf("withdraw: %d %v", code, body)
	}
	if code, body := do(t, ts, "POST", "/v1/lists/OFAC/republish", "off1", map[string]any{
		"base_version": 1, "version": 2,
		"entries": []map[string]any{{
			"entry_id": "E1", "primary_name": "John Smith", "date_of_birth": "1980-01-01",
		}},
	}); code != http.StatusCreated {
		t.Fatalf("republish: %d %v", code, body)
	} else if len(body["reopened_case_ids"].([]any)) != 1 {
		t.Fatalf("retroactive rescan should reopen the false-positive case: %v", body)
	}

	// 审计重放仅复核人可访问
	if code, _ := do(t, ts, "GET", "/v1/audit/replay", "off1", nil); code != http.StatusForbidden {
		t.Fatalf("officer audit = %d, want 403", code)
	}
	if code, body := do(t, ts, "GET", "/v1/audit/replay", "rev1", nil); code != http.StatusOK {
		t.Fatalf("reviewer audit = %d", code)
	} else if body["integrity_ok"] != true {
		t.Fatalf("audit integrity: %v", body["integrity_ok"])
	}
}

func TestPauseAndMergeGuardsOverHTTP(t *testing.T) {
	ts, closeFn := bootServer(t)
	defer closeFn()

	mustCode := func(want, code int, body map[string]any) {
		t.Helper()
		if code != want {
			t.Fatalf("status = %d, want %d: %v", code, want, body)
		}
	}
	want := func(want int, method, path, actor string, body any) {
		t.Helper()
		code, got := do(t, ts, method, path, actor, body)
		mustCode(want, code, got)
	}

	for _, o := range []map[string]string{
		{"officer_id": "off1", "name": "O", "role": "officer"},
		{"officer_id": "rev1", "name": "R", "role": "reviewer"},
	} {
		want(http.StatusCreated, "POST", "/v1/admin/officers", "", o)
	}
	want(http.StatusOK, "PUT", "/v1/customers", "off1",
		map[string]any{"customer_id": "C1", "legal_name": "John Smith"})
	want(http.StatusCreated, "POST", "/v1/lists/L/publish", "off1", map[string]any{
		"version": 1, "entries": []map[string]any{{"entry_id": "E1", "primary_name": "John Smith"}},
	})
	want(http.StatusOK, "POST", "/v1/screen", "off1",
		map[string]any{"customer_ids": []string{"C1"}})
	_, body := do(t, ts, "GET", "/v1/cases", "off1", nil)
	caseID := body["cases"].([]any)[0].(map[string]any)["case_id"].(string)

	// 暂停原因必填 → 400
	if code, b := do(t, ts, "POST", "/v1/cases/"+caseID+"/pause", "off1", map[string]any{"reason": " "}); code != http.StatusBadRequest {
		t.Fatalf("empty pause reason = %d %v", code, b)
	}
	want(http.StatusOK, "POST", "/v1/cases/"+caseID+"/pause", "off1",
		map[string]any{"reason": "awaiting docs"})
	// 暂停中再暂停 → 409
	want(http.StatusConflict, "POST", "/v1/cases/"+caseID+"/pause", "off1",
		map[string]any{"reason": "again"})
	// 暂停中解除 → 409
	want(http.StatusConflict, "POST", "/v1/cases/"+caseID+"/resolve", "rev1",
		map[string]any{"decision": "true_positive", "evidence_summary": "x"})
	want(http.StatusOK, "POST", "/v1/cases/"+caseID+"/resume", "off1", nil)
	// 操作不存在的案件 → 404
	want(http.StatusNotFound, "POST", "/v1/cases/CASE-9999/resume", "off1", nil)

	// 未知 JSON 字段被拒绝
	raw := []byte(`{"reason":"x","bogus":1}`)
	req, _ := http.NewRequest("POST", ts.URL+"/v1/cases/"+caseID+"/pause", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Officer-ID", "off1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown fields = %d, want 400", resp.StatusCode)
	}
}
