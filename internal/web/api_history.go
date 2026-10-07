package web

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/stats"
)

type toolCallDTO struct {
	Tool        string    `json:"tool"`
	ErrorMsg    string    `json:"errorMsg,omitempty"`
	SessionName string    `json:"sessionName,omitempty"`
	Workspace   string    `json:"workspace,omitempty"`
	CalledAt    time.Time `json:"calledAt"`
	DurationMs  int64     `json:"durationMs"`
	Success     bool      `json:"success"`
}

type historyItemDTO struct {
	Seq          int64        `json:"seq"`
	At           time.Time    `json:"at"`
	CallID       string       `json:"callId"`
	Workspace    string       `json:"workspace"`
	Path         string       `json:"path"`
	From         string       `json:"from,omitempty"`
	Kind         string       `json:"kind"`
	Op           string       `json:"op"`
	Tool         string       `json:"tool"`
	SessionID    string       `json:"sessionId,omitempty"`
	SessionName  string       `json:"sessionName,omitempty"`
	LogicalAgent string       `json:"logicalAgent,omitempty"`
	ClientName   string       `json:"clientName,omitempty"`
	BeforeSHA    string       `json:"beforeSha,omitempty"`
	AfterSHA     string       `json:"afterSha,omitempty"`
	BeforeSize   int64        `json:"beforeSize"`
	AfterSize    int64        `json:"afterSize"`
	BeforeExists bool         `json:"beforeExists"`
	AfterExists  bool         `json:"afterExists"`
	Added        int          `json:"added"`
	Removed      int          `json:"removed"`
	Redactions   int          `json:"redactions"`
	Content      string       `json:"content"`
	RevertsSeq   int64        `json:"revertsSeq,omitempty"`
	Reason       string       `json:"reason,omitempty"`
	GapBefore    bool         `json:"gapBefore"`
	GapDropped   bool         `json:"gapDropped"`
	Call         *toolCallDTO `json:"call,omitempty"`
}

type historyListDTO struct {
	Workspace  string           `json:"workspace"`
	Workspaces []workspaceRef   `json:"workspaces"`
	Changes    []historyItemDTO `json:"changes"`
}

type historyDetailDTO struct {
	Seq     int64            `json:"seq,omitempty"`
	CallID  string           `json:"callId,omitempty"`
	Entry   *historyItemDTO  `json:"entry,omitempty"`
	Diff    string           `json:"diff,omitempty"`
	Entries []historyItemDTO `json:"entries"`
	Diffs   []string         `json:"diffs"`
	Call    *toolCallDTO     `json:"call,omitempty"`
}

func toHistoryItemDTO(e history.Entry) historyItemDTO {
	dto := historyItemDTO{
		Seq:          e.Seq,
		At:           e.At,
		CallID:       e.CallID,
		Workspace:    e.Workspace,
		Path:         e.Path,
		From:         e.From,
		Kind:         string(e.Kind),
		Op:           string(e.Op),
		Tool:         e.Tool,
		SessionID:    e.SessionID,
		SessionName:  e.SessionName,
		LogicalAgent: e.LogicalAgent,
		ClientName:   e.ClientName,
		BeforeSize:   e.BeforeSize,
		AfterSize:    e.AfterSize,
		BeforeExists: e.BeforeExists,
		AfterExists:  e.AfterExists,
		Added:        e.Added,
		Removed:      e.Removed,
		Redactions:   e.Redactions,
		Content:      string(e.Content),
		RevertsSeq:   e.RevertsSeq,
		Reason:       e.Reason,
		GapBefore:    e.GapBefore,
		GapDropped:   e.GapDropped,
	}
	if len(e.BeforeSHA) > 0 {
		dto.BeforeSHA = hex.EncodeToString(e.BeforeSHA)
	}
	if len(e.AfterSHA) > 0 {
		dto.AfterSHA = hex.EncodeToString(e.AfterSHA)
	}
	return dto
}

func enrichHistoryCalls(changes []historyItemDTO, statsDB *stats.DB) {
	if statsDB == nil {
		return
	}
	cache := make(map[string]*toolCallDTO)
	for i := range changes {
		cid := changes[i].CallID
		if cid == "" {
			continue
		}
		if callDTO, ok := cache[cid]; ok {
			changes[i].Call = callDTO
			continue
		}
		summary, ok, err := statsDB.CallByID(cid)
		if err == nil && ok {
			callDTO := toToolCallDTO(summary)
			cache[cid] = callDTO
			changes[i].Call = callDTO
		} else {
			cache[cid] = nil
		}
	}
}

func toToolCallDTO(s stats.CallSummary) *toolCallDTO {
	return &toolCallDTO{
		Tool:        s.Tool,
		ErrorMsg:    s.ErrorMsg,
		SessionName: s.SessionName,
		Workspace:   s.Workspace,
		CalledAt:    s.CalledAt,
		DurationMs:  s.DurationMs,
		Success:     s.Success,
	}
}

func parseHistoryFilter(r *http.Request) (history.Filter, string, bool) {
	q := r.URL.Query()
	wsParam := q.Get("workspace")
	all := q.Get("all") == "true" || q.Get("all") == "1"

	var ws string
	if all {
		ws = ""
	} else {
		resolved, ok := resolveWorkspace(wsParam)
		if !ok {
			return history.Filter{}, wsParam, false
		}
		ws = resolved
	}

	filter := history.Filter{
		Workspace: ws,
		All:       all,
		SessionID: q.Get("session"),
		Agent:     q.Get("agent"),
		Tool:      q.Get("tool"),
		File:      q.Get("file"),
	}

	if lim := q.Get("limit"); lim != "" {
		if n, err := strconv.Atoi(lim); err == nil && n > 0 {
			filter.Limit = min(n, 500)
		}
	}
	if s := q.Get("since"); s != "" {
		filter.Since = parseTimeParam(s)
	}
	if u := q.Get("until"); u != "" {
		filter.Until = parseTimeParam(u)
	}
	return filter, ws, true
}

func parseTimeParam(s string) time.Time {
	if ms, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(ms)
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// handleHistory returns write-diff history items matching query criteria.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	filter, ws, ok := parseHistoryFilter(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown workspace: "+ws)
		return
	}

	out := historyListDTO{
		Workspace:  ws,
		Workspaces: activeWorkspaces(),
		Changes:    []historyItemDTO{},
	}
	// No workspace given and none active: an empty Workspace filter would list
	// every workspace under a "" label. Only all=true asks for that.
	if ws == "" && !filter.All {
		writeJSON(w, out)
		return
	}

	rdr, err := history.OpenReadOnly()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "opening history: "+err.Error())
		return
	}
	if rdr == nil {
		writeJSON(w, out)
		return
	}
	defer rdr.Close()

	entries, err := rdr.List(filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "listing history: "+err.Error())
		return
	}

	for _, e := range entries {
		out.Changes = append(out.Changes, toHistoryItemDTO(e))
	}

	statsDB, _ := stats.SharedReadOnly()
	enrichHistoryCalls(out.Changes, statsDB)

	writeJSON(w, out)
}

func historyDetailBySeq(rdr *history.Reader, statsDB *stats.DB, seq int64) (*historyDetailDTO, error) {
	entry, diff, err := rdr.Get(seq)
	if err != nil {
		return nil, err
	}
	dto := toHistoryItemDTO(entry)
	dtos := []historyItemDTO{dto}
	enrichHistoryCalls(dtos, statsDB)
	return &historyDetailDTO{
		Seq:     seq,
		CallID:  entry.CallID,
		Entry:   &dtos[0],
		Diff:    diff,
		Entries: dtos,
		Diffs:   []string{diff},
		Call:    dtos[0].Call,
	}, nil
}

func historyDetailByCall(rdr *history.Reader, statsDB *stats.DB, callID string) (*historyDetailDTO, error) {
	entries, diffs, err := rdr.ByCall(callID)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, sql.ErrNoRows
	}
	dtos := make([]historyItemDTO, 0, len(entries))
	for _, e := range entries {
		dtos = append(dtos, toHistoryItemDTO(e))
	}
	enrichHistoryCalls(dtos, statsDB)

	var callDTO *toolCallDTO
	if statsDB != nil {
		if summary, ok, _ := statsDB.CallByID(callID); ok {
			callDTO = toToolCallDTO(summary)
		}
	}
	if callDTO == nil && len(dtos) > 0 {
		callDTO = dtos[0].Call
	}

	res := &historyDetailDTO{
		CallID:  callID,
		Entries: dtos,
		Diffs:   diffs,
		Call:    callDTO,
	}
	if len(dtos) == 1 {
		res.Seq = dtos[0].Seq
		res.Entry = &dtos[0]
		res.Diff = diffs[0]
	}
	return res, nil
}

// handleHistoryDetail returns entry/diff and tool-call detail for a sequence number or call ID.
func (s *Server) handleHistoryDetail(w http.ResponseWriter, r *http.Request) {
	target := r.PathValue("target")
	if target == "" {
		writeError(w, http.StatusBadRequest, "missing history target")
		return
	}

	rdr, err := history.OpenReadOnly()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "opening history: "+err.Error())
		return
	}
	if rdr == nil {
		writeError(w, http.StatusNotFound, "history not found")
		return
	}
	defer rdr.Close()

	statsDB, _ := stats.SharedReadOnly()

	if seq, seqErr := strconv.ParseInt(target, 10, 64); seqErr == nil && seq > 0 {
		res, err := historyDetailBySeq(rdr, statsDB, seq)
		if err == nil {
			writeJSON(w, res)
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, "reading history: "+err.Error())
			return
		}
	}

	res, err := historyDetailByCall(rdr, statsDB, target)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "history entry not found: "+target)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reading history by call: "+err.Error())
		return
	}
	writeJSON(w, res)
}
