package web

import (
	"net/http"
	"time"

	"github.com/plumbkit/plumb/internal/render"
	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
)

type sessionDTO struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Client        string    `json:"client"`
	ClientVersion string    `json:"clientVersion"`
	Language      string    `json:"language"`
	Adapters      []string  `json:"adapters"`
	Folder        string    `json:"folder"`
	FolderShort   string    `json:"folderShort"`
	Health        string    `json:"health"`
	HealthMessage string    `json:"healthMessage"`
	Synthetic     bool      `json:"synthetic"`
	StartedAt     time.Time `json:"startedAt"`
	LastSeen      string    `json:"lastSeen"`
	LastSeenAt    time.Time `json:"lastSeenAt"`
	Calls         int64     `json:"calls"`
}

// handleSessions lists the active sessions with their per-session call counts.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	infos, err := session.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "listing sessions: "+err.Error())
		return
	}

	db, _ := stats.SharedReadOnly()

	out := make([]sessionDTO, 0, len(infos))
	for _, info := range infos {
		if !info.EndedAt.IsZero() {
			continue
		}
		d := sessionDTO{
			ID: info.ID, Name: info.Name, Client: info.ClientName,
			ClientVersion: info.ClientVersion, Language: info.Language,
			Adapters: info.Adapters, Folder: info.Folder,
			FolderShort: render.ContractPath(info.Folder),
			Health:      info.Health, HealthMessage: info.HealthMessage,
			Synthetic: info.Synthetic, StartedAt: info.StartedAt,
			LastSeenAt: info.LastSeenAt,
		}
		if !info.LastSeenAt.IsZero() {
			d.LastSeen = render.HumanAge(info.LastSeenAt)
		}
		if db != nil {
			d.Calls = sessionCalls(db, info)
		}
		out = append(out, d)
	}
	writeJSON(w, out)
}

func sessionCalls(db *stats.DB, info session.Info) int64 {
	rows, err := db.Summary(stats.Filter{SessionID: info.ID})
	if err != nil {
		return 0
	}
	var total int64
	for _, t := range rows {
		total += t.Calls
	}
	return total
}
