// Package httpapi 把案件台用例暴露为 JSON HTTP 接口。
//
// 鉴权为请求头 X-Officer-ID（生产环境应由网关换成签名令牌）；
// 观看与操作权限按人员名册中的角色执行，返回内容经 view 包按角色裁剪。
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"example.com/09181/q012/internal/domain"
	"example.com/09181/q012/internal/service"
	"example.com/09181/q012/internal/view"
)

// Server 持有应用服务与路由。
type Server struct {
	svc *service.Service
	mux *http.ServeMux
}

// NewServer 构造路由完备的 HTTP 服务。
func NewServer(svc *service.Service) *Server {
	s := &Server{svc: svc, mux: http.NewServeMux()}
	s.routes()
	return s
}

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	m.HandleFunc("/v1/admin/officers", s.handleProvisionOfficer)
	m.HandleFunc("/v1/customers", s.handleUpsertCustomer)
	m.HandleFunc("/v1/lists/", s.handleListSubroutes)
	m.HandleFunc("/v1/hits/batch", s.handleIngestHits)
	m.HandleFunc("/v1/screen", s.handleScreen)
	m.HandleFunc("/v1/cases", s.handleListCases)
	m.HandleFunc("/v1/cases/", s.handleCaseSubroutes)
	m.HandleFunc("/v1/reminders/pump", s.handlePumpReminders)
	m.HandleFunc("/v1/audit/replay", s.handleAuditReplay)
}

// ---------------------------------------------------------------------------
// DTO
// ---------------------------------------------------------------------------

type officerDTO struct {
	OfficerID string `json:"officer_id"`
	Name      string `json:"name"`
	Role      string `json:"role"`
}

type customerDTO struct {
	CustomerID  string           `json:"customer_id"`
	LegalName   string           `json:"legal_name"`
	Aliases     []string         `json:"aliases"`
	DateOfBirth string           `json:"date_of_birth"`
	Nationality string           `json:"nationality"`
	IDDocuments []string         `json:"id_documents"`
	Addresses   []domain.Address `json:"addresses"`
	Timezone    string           `json:"timezone"`
}

type listEntryDTO struct {
	EntryID     string   `json:"entry_id"`
	PrimaryName string   `json:"primary_name"`
	Aliases     []string `json:"aliases"`
	DateOfBirth string   `json:"date_of_birth"`
	Nationality string   `json:"nationality"`
	IDDocuments []string `json:"id_documents"`
	Program     string   `json:"program"`
}

type publishListDTO struct {
	Version     int            `json:"version"`
	Entries     []listEntryDTO `json:"entries"`
	PublishedAt string         `json:"published_at,omitempty"`
}

type republishListDTO struct {
	BaseVersion int            `json:"base_version"`
	Version     int            `json:"version"`
	Entries     []listEntryDTO `json:"entries"`
	At          string         `json:"at,omitempty"`
}

type hitDTO struct {
	CustomerID  string               `json:"customer_id"`
	SubjectKey  string               `json:"subject_key"`
	ListID      string               `json:"list_id"`
	EntryID     string               `json:"entry_id"`
	MatchedName string               `json:"matched_name"`
	Score       float64              `json:"score"`
	Reasons     []domain.MatchReason `json:"reasons"`
	OccurredAt  string               `json:"occurred_at,omitempty"`
}

type batchHitsDTO struct {
	Hits []hitDTO `json:"hits"`
}

type screenDTO struct {
	CustomerIDs []string `json:"customer_ids"`
}

type assignDTO struct {
	OfficerID string `json:"officer_id"`
}

type mergeDTO struct {
	SurvivorCaseID string `json:"survivor_case_id"`
	AbsorbedCaseID string `json:"absorbed_case_id"`
}

type pauseDTO struct {
	Reason string `json:"reason"`
}

type noteDTO struct {
	Content string `json:"content"`
}

type resolveDTO struct {
	Decision          string `json:"decision"`
	EvidenceSummary   string `json:"evidence_summary"`
	DuplicateOfCaseID string `json:"duplicate_of_case_id,omitempty"`
}

// ---------------------------------------------------------------------------
// 处理函数
// ---------------------------------------------------------------------------

func (s *Server) handleProvisionOfficer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in officerDTO
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	err := s.svc.RegisterOfficer(domain.Officer{
		OfficerID: in.OfficerID, Name: in.Name, Role: domain.Role(in.Role),
	})
	if err != nil {
		writeDomainError(w, "", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "provisioned", "officer_id": in.OfficerID})
}

func (s *Server) handleUpsertCustomer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, err := s.requireActor(r, domain.RoleOfficer, domain.RoleReviewer); err != nil {
		writeDomainError(w, "", err)
		return
	}
	var in customerDTO
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	err := s.svc.UpsertCustomer(domain.Customer{
		CustomerID: in.CustomerID, LegalName: in.LegalName, Aliases: in.Aliases,
		DateOfBirth: in.DateOfBirth, Nationality: in.Nationality,
		IDDocuments: in.IDDocuments, Addresses: in.Addresses, Timezone: in.Timezone,
	})
	if err != nil {
		writeDomainError(w, "", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "accepted", "customer_id": in.CustomerID})
}

func (s *Server) handleListSubroutes(w http.ResponseWriter, r *http.Request) {
	// /v1/lists/{id}/publish|withdraw|republish
	rest := strings.TrimPrefix(r.URL.Path, "/v1/lists/")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" {
		writeError(w, http.StatusNotFound, "unknown list route")
		return
	}
	listID, action := parts[0], parts[1]
	if _, err := s.requireActor(r, domain.RoleOfficer, domain.RoleReviewer); err != nil {
		writeDomainError(w, "", err)
		return
	}

	switch action {
	case "publish":
		var in publishListDTO
		if err := decode(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		at, _ := parseTime(in.PublishedAt)
		if err := s.svc.PublishList(listID, in.Version, toEntries(in.Entries), at); err != nil {
			writeDomainError(w, "", err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"list_id": listID, "version": in.Version, "status": "active"})
	case "withdraw":
		version, err := queryVersion(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.svc.WithdrawList(listID, version); err != nil {
			writeDomainError(w, "", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"list_id": listID, "version": version, "status": "withdrawn"})
	case "republish":
		var in republishListDTO
		if err := decode(r, &in); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		at, _ := parseTime(in.At)
		rep, err := s.svc.RepublishList(listID, in.BaseVersion, in.Version, toEntries(in.Entries), at)
		if err != nil {
			writeDomainError(w, "", err)
			return
		}
		writeJSON(w, http.StatusCreated, rep)
	default:
		writeError(w, http.StatusNotFound, "unknown list action")
	}
}

func (s *Server) handleIngestHits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, err := s.requireActor(r, domain.RoleOfficer, domain.RoleReviewer); err != nil {
		writeDomainError(w, "", err)
		return
	}
	var in batchHitsDTO
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	inputs := make([]service.HitInput, 0, len(in.Hits))
	for _, h := range in.Hits {
		at, _ := parseTime(h.OccurredAt)
		inputs = append(inputs, service.HitInput{
			CustomerID: h.CustomerID, SubjectKey: h.SubjectKey, ListID: h.ListID,
			EntryID: h.EntryID, MatchedName: h.MatchedName, Score: h.Score,
			Reasons: h.Reasons, OccurredAt: at,
		})
	}
	rep, err := s.svc.IngestHits(inputs)
	if err != nil {
		writeDomainError(w, "", err)
		return
	}
	writeJSON(w, http.StatusAccepted, rep)
}

func (s *Server) handleScreen(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, err := s.requireActor(r, domain.RoleOfficer, domain.RoleReviewer); err != nil {
		writeDomainError(w, "", err)
		return
	}
	var in screenDTO
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rep, err := s.svc.ScreenCustomers(in.CustomerIDs)
	if err != nil {
		writeDomainError(w, "", err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleListCases(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	actor, err := s.requireActor(r, domain.RoleAgent, domain.RoleOfficer, domain.RoleReviewer)
	if err != nil {
		writeDomainError(w, "", err)
		return
	}
	builder := view.ForRole(actor.Role)
	statuses := r.URL.Query()["status"]
	var cases []domain.CaseStatus
	for _, st := range statuses {
		cases = append(cases, domain.CaseStatus(st))
	}
	list := s.svc.Projection().Cases(cases...)
	out := make([]view.CaseView, 0, len(list))
	for _, c := range list {
		snap, ok := s.svc.Projection().CaseSnapshot(c.CaseID)
		if !ok {
			continue
		}
		var cust *domain.Customer
		if c2, ok := s.svc.Projection().Customer(c.CustomerID); ok {
			cust = &c2
		}
		out = append(out, builder.Case(snap, cust))
	}
	writeJSON(w, http.StatusOK, map[string]any{"cases": out})
}

func (s *Server) handleCaseSubroutes(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/cases/")
	parts := strings.Split(rest, "/")
	if len(parts) == 1 && r.Method == http.MethodGet {
		s.getCase(w, r, parts[0])
		return
	}
	if len(parts) != 2 {
		writeError(w, http.StatusNotFound, "unknown case route")
		return
	}
	caseID, action := parts[0], parts[1]
	switch action {
	case "assign":
		s.assignCase(w, r, caseID)
	case "merge":
		s.mergeCases(w, r, caseID)
	case "pause":
		s.pauseCase(w, r, caseID)
	case "resume":
		s.resumeCase(w, r, caseID)
	case "notes":
		s.addNote(w, r, caseID)
	case "resolve":
		s.resolveCase(w, r, caseID)
	default:
		writeError(w, http.StatusNotFound, "unknown case action")
	}
}

func (s *Server) getCase(w http.ResponseWriter, r *http.Request, caseID string) {
	actor, err := s.requireActor(r, domain.RoleAgent, domain.RoleOfficer, domain.RoleReviewer)
	if err != nil {
		writeDomainError(w, actor.Role, err)
		return
	}
	snap, ok := s.svc.Projection().CaseSnapshot(caseID)
	if !ok {
		writeDomainError(w, actor.Role, domain.ErrCaseNotFound)
		return
	}
	var cust *domain.Customer
	if c, ok := s.svc.Projection().Customer(snap.Case.CustomerID); ok {
		cust = &c
	}
	writeJSON(w, http.StatusOK, view.ForRole(actor.Role).Case(snap, cust))
}

func (s *Server) assignCase(w http.ResponseWriter, r *http.Request, caseID string) {
	actor, err := s.requireActor(r, domain.RoleOfficer, domain.RoleReviewer)
	if err != nil {
		writeDomainError(w, "", err)
		return
	}
	var in assignDTO
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.svc.AssignCase(actor.OfficerID, caseID, in.OfficerID); err != nil {
		writeDomainError(w, "", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "assigned", "case_id": caseID, "officer_id": in.OfficerID})
}

func (s *Server) mergeCases(w http.ResponseWriter, r *http.Request, _ string) {
	actor, err := s.requireActor(r, domain.RoleOfficer, domain.RoleReviewer)
	if err != nil {
		writeDomainError(w, "", err)
		return
	}
	var in mergeDTO
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.svc.MergeCases(actor.OfficerID, in.SurvivorCaseID, in.AbsorbedCaseID); err != nil {
		writeDomainError(w, "", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "merged", "survivor_case_id": in.SurvivorCaseID, "absorbed_case_id": in.AbsorbedCaseID,
	})
}

func (s *Server) pauseCase(w http.ResponseWriter, r *http.Request, caseID string) {
	actor, err := s.requireActor(r, domain.RoleOfficer, domain.RoleReviewer)
	if err != nil {
		writeDomainError(w, "", err)
		return
	}
	var in pauseDTO
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.svc.PauseCase(actor.OfficerID, caseID, in.Reason); err != nil {
		writeDomainError(w, "", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "paused", "case_id": caseID})
}

func (s *Server) resumeCase(w http.ResponseWriter, r *http.Request, caseID string) {
	actor, err := s.requireActor(r, domain.RoleOfficer, domain.RoleReviewer)
	if err != nil {
		writeDomainError(w, "", err)
		return
	}
	if err := s.svc.ResumeCase(actor.OfficerID, caseID); err != nil {
		writeDomainError(w, "", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resumed", "case_id": caseID})
}

func (s *Server) addNote(w http.ResponseWriter, r *http.Request, caseID string) {
	actor, err := s.requireActor(r, domain.RoleOfficer, domain.RoleReviewer)
	if err != nil {
		writeDomainError(w, "", err)
		return
	}
	var in noteDTO
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.svc.AddNote(actor.OfficerID, caseID, in.Content); err != nil {
		writeDomainError(w, "", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "recorded", "case_id": caseID})
}

func (s *Server) resolveCase(w http.ResponseWriter, r *http.Request, caseID string) {
	reviewer, err := s.requireActor(r, domain.RoleReviewer)
	if err != nil {
		writeDomainError(w, "", err)
		return
	}
	var in resolveDTO
	if err := decode(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	err = s.svc.ResolveCase(reviewer.OfficerID, service.DispositionInput{
		CaseID: caseID, Decision: domain.Decision(in.Decision),
		EvidenceSummary: in.EvidenceSummary, DuplicateOfCaseID: in.DuplicateOfCaseID,
	})
	if err != nil {
		writeDomainError(w, "", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "resolved", "case_id": caseID, "decision": in.Decision})
}

func (s *Server) handlePumpReminders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, err := s.requireActor(r, domain.RoleOfficer, domain.RoleReviewer); err != nil {
		writeDomainError(w, "", err)
		return
	}
	fired, err := s.svc.PumpReminders(context.Background())
	if err != nil {
		writeDomainError(w, "", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"fired": fired})
}

func (s *Server) handleAuditReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	actor, err := s.requireActor(r, domain.RoleReviewer)
	if err != nil {
		writeDomainError(w, "", err)
		return
	}
	rep, err := s.svc.AuditReplay()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func (s *Server) requireActor(r *http.Request, roles ...domain.Role) (domain.Officer, error) {
	id := strings.TrimSpace(r.Header.Get("X-Officer-ID"))
	if id == "" {
		return domain.Officer{}, fmt.Errorf("%w: X-Officer-ID header is required", domain.ErrOfficerNotFound)
	}
	o, ok := s.svc.Projection().Officer(id)
	if !ok {
		return domain.Officer{}, domain.ErrOfficerNotFound
	}
	for _, role := range roles {
		if o.Role == role {
			return o, nil
		}
	}
	return o, domain.ErrRoleNotPermitted
}

func toEntries(in []listEntryDTO) []domain.ListEntry {
	out := make([]domain.ListEntry, len(in))
	for i, e := range in {
		out[i] = domain.ListEntry{
			EntryID: e.EntryID, PrimaryName: e.PrimaryName, Aliases: e.Aliases,
			DateOfBirth: e.DateOfBirth, Nationality: e.Nationality,
			IDDocuments: e.IDDocuments, Program: e.Program,
		}
	}
	return out
}

func queryVersion(r *http.Request) (int, error) {
	v := strings.TrimSpace(r.URL.Query().Get("version"))
	if v == "" {
		return 0, errors.New("version query parameter is required")
	}
	var n int
	for _, ch := range v {
		if ch < '0' || ch > '9' {
			return 0, errors.New("version must be an integer")
		}
		n = n*10 + int(ch-'0')
	}
	if n <= 0 {
		return 0, errors.New("version must be positive")
	}
	return n, nil
}

func parseTime(s string) (time.Time, error) {
	if strings.TrimSpace(s) == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeDomainError(w http.ResponseWriter, role domain.Role, err error) {
	status := http.StatusInternalServerError
	msg := err.Error()
	switch {
	case errors.Is(err, domain.ErrInvalidInput):
		status = http.StatusBadRequest
	case errors.Is(err, domain.ErrOfficerNotFound):
		status = http.StatusUnauthorized
		if role == domain.RoleAgent {
			msg = string(domain.ErrOfficerNotFound)
		}
	case errors.Is(err, domain.ErrRoleNotPermitted):
		status = http.StatusForbidden
	case errors.Is(err, domain.ErrCaseNotFound), errors.Is(err, domain.ErrCustomerNotFound),
		errors.Is(err, domain.ErrListNotFound), errors.Is(err, domain.ErrListVersionNotFound):
		status = http.StatusNotFound
	case errors.Is(err, domain.ErrCaseTerminal), errors.Is(err, domain.ErrCasePaused),
		errors.Is(err, domain.ErrCaseNotPaused), errors.Is(err, domain.ErrCaseAbsorbed),
		errors.Is(err, domain.ErrCasesAlreadyRelated), errors.Is(err, domain.ErrListAlreadyExists),
		errors.Is(err, domain.ErrListVersionInactive), errors.Is(err, domain.ErrListNotWithdrawn),
		errors.Is(err, domain.ErrReviewerRequired), errors.Is(err, domain.ErrEvidenceRequired),
		errors.Is(err, domain.ErrDecisionInvalid):
		status = http.StatusConflict
	}
	if role == domain.RoleAgent {
		msg = view.MaskError(role, msg)
	}
	writeError(w, status, msg)
}
