package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"example.com/09181/q012/internal/screening"
)

// server 把案件台领域服务暴露为最小 JSON API。
// 认证在演示环境用 X-Actor + X-Role 头；生产部署应替换为真实身份提供方。
type server struct {
	svc  *screening.Service
	view func() (*screening.View, error)
}

func newServer(storePath string) (*server, error) {
	var store screening.EventStore
	var err error
	if storePath != "" {
		store, err = screening.OpenFileEventStore(storePath)
		if err != nil {
			return nil, err
		}
	} else {
		store = screening.NewMemoryEventStore()
	}
	authz := demoAuthorizer{}
	svc := screening.NewService(store, authz)
	return &server{
		svc: svc,
		view: func() (*screening.View, error) {
			p, err := screening.LoadReplay(store)
			if err != nil {
				return nil, err
			}
			return screening.NewView(p, authz), nil
		},
	}, nil
}

// demoAuthorizer 演示用：actor 名即角色（officer/reviewer/agent）。
type demoAuthorizer struct{}

func (demoAuthorizer) RoleOf(actor string) screening.Role {
	switch actor {
	case "officer", "reviewer", "agent":
		return screening.Role(actor)
	}
	return screening.RoleAgent
}

func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/customers", s.importCustomers)
	mux.HandleFunc("/list-entries", s.importList)
	mux.HandleFunc("/hits", s.ingest)
	mux.HandleFunc("/cases", s.listCases)
	mux.HandleFunc("/cases/", s.caseAction)
	mux.HandleFunc("/list/", s.listAction)
	mux.HandleFunc("/push-due", s.pushDue)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case strings.Contains(err.Error(), "forbidden"):
		status = http.StatusForbidden
	case strings.Contains(err.Error(), "not found"):
		status = http.StatusNotFound
	case strings.Contains(err.Error(), "conflict"):
		status = http.StatusConflict
	case strings.Contains(err.Error(), "invalid"), strings.Contains(err.Error(), "boundary"),
		strings.Contains(err.Error(), "reviewer"), strings.Contains(err.Error(), "suspended"),
		strings.Contains(err.Error(), "still has active"):
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func actor(r *http.Request) string {
	if a := r.Header.Get("X-Actor"); a != "" {
		return a
	}
	return r.Header.Get("X-Role") // 演示：直接用角色名当 actor
}

func (s *server) importCustomers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Customers []screening.Customer `json:"customers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.svc.ImportCustomers(actor(r), in.Customers); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (s *server) importList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Entries []screening.ListEntry `json:"entries"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := s.svc.ImportListEntries(actor(r), in.Entries); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (s *server) ingest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		BatchID string          `json:"batchId"`
		Hits    []screening.Hit `json:"hits"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	for i := range in.Hits {
		if in.Hits[i].ScreenedAt.IsZero() {
			in.Hits[i].ScreenedAt = time.Now().UTC()
		}
	}
	ids, plan, err := s.svc.IngestBatch(actor(r), in.BatchID, in.Hits)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"caseIds": ids, "plan": plan})
}

func (s *server) listCases(w http.ResponseWriter, r *http.Request) {
	v, err := s.view()
	if err != nil {
		writeErr(w, err)
		return
	}
	cases, err := v.Cases(actor(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cases)
}

// POST /cases/{id}/merge|assign|suspend|resume|disposition|detach|notes
func (s *server) caseAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/cases/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	caseID, action := parts[0], parts[1]
	var body map[string]json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&body)
	str := func(k string) string {
		var v string
		if b, ok := body[k]; ok {
			_ = json.Unmarshal(b, &v)
		}
		return v
	}
	tme := func(k string) time.Time {
		var v string
		if b, ok := body[k]; ok {
			_ = json.Unmarshal(b, &v)
			t, _ := time.Parse(time.RFC3339, v)
			return t.UTC()
		}
		return time.Time{}
	}

	var err error
	switch action {
	case "merge":
		var in struct {
			SourceCases []string `json:"sourceCases"`
			Rule        string   `json:"rule"`
		}
		_ = json.Unmarshal(body["sourceCases"], &in.SourceCases)
		in.Rule = str("rule")
		err = s.svc.MergeCases(actor(r), caseID, in.SourceCases, in.Rule)
	case "assign":
		err = s.svc.AssignCase(actor(r), caseID, str("officer"), tme("reminderDue"), tme("escalationDue"))
	case "suspend":
		_, err = s.svc.SuspendCase(actor(r), caseID, str("reason"))
	case "resume":
		err = s.svc.ResumeCase(actor(r), caseID, tme("suspendedAt"))
	case "disposition":
		var disp screening.Disposition
		_ = json.Unmarshal(body["disposition"], &disp)
		err = s.svc.RecordDisposition(actor(r), caseID, disp, str("reviewer"), str("evidence"))
	case "retroactive-clear":
		err = s.svc.ResolveRetroactive(actor(r), caseID, str("reviewer"), str("evidence"))
	case "detach":
		err = s.svc.DetachHit(actor(r), caseID, str("hitId"), str("reviewer"), str("evidence"))
	case "notes":
		err = s.svc.AddNote(actor(r), caseID, str("noteId"), str("content"))
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

// POST /list/{id}/withdraw | /list/{id}/republish
func (s *server) listAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/list/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	listID, action := parts[0], parts[1]
	var in struct {
		Version string               `json:"version"`
		Entry   screening.ListEntry  `json:"entry"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	var err error
	switch action {
	case "withdraw":
		err = s.svc.WithdrawListEntry(actor(r), listID, in.Version)
	case "republish":
		in.Entry.ID = listID
		if in.Version != "" {
			in.Entry.Version = in.Version
		}
		err = s.svc.RepublishListEntry(actor(r), in.Entry)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (s *server) pushDue(w http.ResponseWriter, r *http.Request) {
	asOf := time.Now().UTC()
	if t := r.URL.Query().Get("asOf"); t != "" {
		parsed, err := time.Parse(time.RFC3339, t)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		asOf = parsed.UTC()
	}
	fired, err := s.svc.PushDue(asOf)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fired": fired})
}
