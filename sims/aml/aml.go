// Package aml simulates the AML screening service with a seeded watchlist and time-driven review cases.
package aml

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bill/ibps-bench/sims/simx"
)

// Server is stateless: a case ID encodes when it was opened and how long it takes to resolve.
type Server struct {
	Sim        *simx.Sim
	FastReview time.Duration
	SlowReview time.Duration
}

func New(sim *simx.Sim) *Server {
	return &Server{Sim: sim, FastReview: time.Second, SlowReview: 30 * time.Second}
}

// Watchlist entries are "BLOCKED PARTY 01" .. "BLOCKED PARTY 20".
func onWatchlist(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, "BLOCKED PARTY ")
	if !ok {
		return "", false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 || n > 20 {
		return "", false
	}
	return "WL-" + rest, true
}

func (s *Server) Routes(mux *http.ServeMux) {
	s.Sim.Control(mux)
	mux.HandleFunc("POST /screen", s.screen)
	mux.HandleFunc("GET /cases/{id}", s.getCase)
}

func (s *Server) screen(w http.ResponseWriter, r *http.Request) {
	s.Sim.Sleep()
	if f := s.Sim.Pick("screen"); f != nil {
		switch f.Mode {
		case "http503":
			http.Error(w, "injected", http.StatusServiceUnavailable)
			return
		case "latency":
			time.Sleep(time.Duration(f.LatencyMs) * time.Millisecond)
		}
	}
	var in struct {
		MsgID        string `json:"msgId"`
		DebtorName   string `json:"debtorName"`
		CreditorName string `json:"creditorName"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	for _, n := range []string{in.DebtorName, in.CreditorName} {
		if id, ok := onWatchlist(n); ok {
			simx.JSON(w, http.StatusOK, map[string]string{"result": "HIT", "listId": id})
			return
		}
	}
	delay := time.Duration(0)
	switch {
	case strings.HasPrefix(in.DebtorName, "REVIEW-FAST"):
		delay = s.FastReview
	case strings.HasPrefix(in.DebtorName, "REVIEW-SLOW"):
		delay = s.SlowReview
	}
	if delay == 0 {
		simx.JSON(w, http.StatusOK, map[string]string{"result": "CLEAR"})
		return
	}
	id := fmt.Sprintf("CASE-%d-%d-%04x", time.Now().UnixMilli(), delay.Milliseconds(), rand.Uint32N(0x10000))
	simx.JSON(w, http.StatusOK, map[string]string{"result": "REVIEW", "caseId": id})
}

// CaseState derives OPEN/CLEARED from the case ID and the current time.
func CaseState(id string, now time.Time) (string, bool) {
	parts := strings.Split(id, "-")
	if len(parts) != 4 || parts[0] != "CASE" {
		return "", false
	}
	opened, err1 := strconv.ParseInt(parts[1], 10, 64)
	delay, err2 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil {
		return "", false
	}
	if now.UnixMilli() >= opened+delay {
		return "CLEARED", true
	}
	return "OPEN", true
}

func (s *Server) getCase(w http.ResponseWriter, r *http.Request) {
	s.Sim.Sleep()
	if f := s.Sim.Pick("cases"); f != nil && f.Mode == "http503" {
		http.Error(w, "injected", http.StatusServiceUnavailable)
		return
	}
	state, ok := CaseState(r.PathValue("id"), time.Now())
	if !ok {
		simx.JSON(w, http.StatusNotFound, map[string]string{"state": "UNKNOWN"})
		return
	}
	simx.JSON(w, http.StatusOK, map[string]string{"state": state})
}
