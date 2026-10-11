package contexthints

// uptake.go — what the ledger says about whether hints are used: invoked,
// emitted, refused and why, and the consumption proxy (PLAN-462 §Phase 3b).
//
// The proxy is deliberately weak and labelled as such: a hint counts as
// consumed when a tool call by the SAME logical agent, soon after, mentions one
// of the selectors the hint named. That is evidence of use, not of
// comprehension, and it can only see plumb's own tool calls: a native read of
// the named file leaves no trace here. A hint whose agent made no attributable
// call in the window is counted as unattributed, never as unconsumed.

import (
	"sort"
	"strings"
	"time"
)

// CallInputs returns the input JSON of up to limit tool calls the logical agent
// made in (from, to], oldest first.
type CallInputs func(agent string, from, to time.Time, limit int) ([]string, error)

// HostUptake is one host's line in an uptake report.
type HostUptake struct {
	Host         string
	Invoked      int
	ByOutcome    map[Outcome]int
	ByDetail     map[string]int // noop/throttled/error reasons
	EmittedBytes int
	// Consumed is emitted hints a later call by the same agent mentioned;
	// Unconsumed had attributable calls that did not; Unattributed had no
	// attributable call in the window at all (an unstamped host, or an agent
	// that went quiet), so it is no evidence either way.
	Consumed     int
	Unconsumed   int
	Unattributed int
	// Agents counts distinct logical agents that were sent at least one hint.
	Agents int
}

// Uptake is a report over every observation since a time.
type Uptake struct {
	Since time.Time
	Hosts []HostUptake
}

// ComputeUptake folds observations into per-host lines, asking calls for the
// consumption proxy over window after each emitted hint, looking at no more than
// maxCalls calls. calls may be nil: consumption is then all unattributed.
func ComputeUptake(obs []Observation, since time.Time, calls CallInputs, window time.Duration, maxCalls int) (Uptake, error) {
	byHost := map[string]*HostUptake{}
	agents := map[string]map[string]bool{}
	for _, o := range obs {
		h := byHost[o.Host]
		if h == nil {
			h = &HostUptake{Host: o.Host, ByOutcome: map[Outcome]int{}, ByDetail: map[string]int{}}
			byHost[o.Host] = h
			agents[o.Host] = map[string]bool{}
		}
		h.Invoked++
		h.ByOutcome[o.Outcome]++
		if o.Detail != "" {
			h.ByDetail[string(o.Outcome)+":"+o.Detail]++
		}
		if o.Outcome != OutcomeEmitted {
			continue
		}
		h.EmittedBytes += o.EmittedBytes
		agent := externalOf(o.SessionID, o.AgentID)
		agents[o.Host][agent] = true
		consumed, attributed, err := consumedBy(o, agent, calls, window, maxCalls)
		if err != nil {
			return Uptake{}, err
		}
		switch {
		case !attributed:
			h.Unattributed++
		case consumed:
			h.Consumed++
		default:
			h.Unconsumed++
		}
	}
	out := Uptake{Since: since}
	for host, h := range byHost {
		h.Agents = len(agents[host])
		out.Hosts = append(out.Hosts, *h)
	}
	sort.Slice(out.Hosts, func(i, j int) bool { return out.Hosts[i].Host < out.Hosts[j].Host })
	return out, nil
}

// externalOf is the identity a logical agent's calls are recorded under:
// the conversation, or `<conversation>/<agent>` for a subagent.
func externalOf(sessionID, agentID string) string {
	if agentID == "" {
		return sessionID
	}
	return sessionID + "/" + agentID
}

func consumedBy(o Observation, agent string, calls CallInputs, window time.Duration, maxCalls int) (consumed, attributed bool, err error) {
	if calls == nil || len(o.Emitted) == 0 {
		return false, false, nil
	}
	inputs, err := calls(agent, o.At, o.At.Add(window), maxCalls)
	if err != nil {
		return false, false, err
	}
	if len(inputs) == 0 {
		return false, false, nil
	}
	for _, in := range inputs {
		for _, sel := range o.Emitted {
			if sel != "" && strings.Contains(in, sel) {
				return true, true, nil
			}
		}
	}
	return false, true, nil
}

// EvictedTotal is how many observations every workspace has lost to the caps
// or to age, so a report can say how much it cannot see. nil-safe.
func (s *Store) EvictedTotal() (int, error) {
	if s == nil {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	err := s.db.QueryRow(`SELECT COALESCE(SUM(evicted), 0) FROM evictions`).Scan(&n)
	return n, err
}
