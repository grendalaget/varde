package api

import (
	"context"
	"encoding/json"

	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
)

// fastLease reports whether the node renews leases via /v1/agent/leases.
func (s *Server) fastLease(ctx context.Context, nodeID string) bool {
	if s.Cfg.Timings.FastLeaseTTLMs <= 0 {
		return false
	}
	st, err := s.Store.GetNodeStatus(ctx, nodeID)
	if err != nil {
		return false
	}
	var c struct {
		FastLease bool `json:"fast_lease"`
	}
	_ = json.Unmarshal([]byte(st.StatusJSON), &c)
	return c.FastLease
}

// leaseTTL is the lease length for the node's executions and whether it is a
// fast lease the agent must renew every LeaseRenewIntervalMs. A fast lease
// only ever shortens the node's profile lease.
func (s *Server) leaseTTL(ctx context.Context, node *store.Node) (ttl int64, fast bool) {
	t := s.Cfg.Timings
	ttl = t.ForProfile(node.LeaseProfile).LeaseTTLMs
	if t.LeaseRenewIntervalMs > 0 && t.FastLeaseTTLMs < ttl && s.fastLease(ctx, node.ID) {
		return t.FastLeaseTTLMs, true
	}
	return ttl, false
}

func (s *Server) AgentRenewLeases(ctx context.Context, req gen.AgentRenewLeasesRequestObject) (gen.AgentRenewLeasesResponseObject, error) {
	node := nodeFrom(ctx)
	if node == nil {
		return nil, errResp(gen.Unauthorized, "no node", nil)
	}
	now := s.Store.NowMs()
	if err := s.Store.TouchNodeSeen(ctx, node.ID, now); err != nil {
		return nil, err
	}
	ttl, _ := s.leaseTTL(ctx, node)
	resp := gen.AgentRenewLeases200JSONResponse{ServerTimeUnixMs: now}
	if req.Body == nil {
		return resp, nil
	}
	for _, l := range req.Body.Executions {
		exp, ok := s.renewLease(ctx, node, l.ExecutionId, l.ServerId, l.Epoch, now, ttl)
		if !ok {
			continue
		}
		resp.Leases = append(resp.Leases, struct {
			ExecutionId          string `json:"execution_id"`
			LeaseExpiresAtUnixMs int64  `json:"lease_expires_at_unix_ms"`
		}{ExecutionId: l.ExecutionId, LeaseExpiresAtUnixMs: exp})
	}
	return resp, nil
}

// renewLease extends one live, unexpired lease the node holds (same checks
// as an execution report, without touching state).
func (s *Server) renewLease(ctx context.Context, node *store.Node, execID, serverID string, epoch, now, ttl int64) (int64, bool) {
	exec, err := s.Store.GetExecution(ctx, execID)
	if err != nil || exec.NodeID != node.ID || exec.Epoch != epoch || exec.ServerID != serverID ||
		exec.EndedAt != nil || exec.LeaseExpiresAt <= now {
		return 0, false
	}
	if exec.Role == "" || exec.Role == "serving" {
		active, err := s.Store.ActiveExecution(ctx, serverID)
		if err != nil || active.ID != exec.ID {
			return 0, false
		}
	}
	exp := now + ttl
	if exp <= exec.LeaseExpiresAt {
		// never shorten (start grace covers preparing/restoring)
		return exec.LeaseExpiresAt, true
	}
	ok, err := s.Store.ExtendLease(ctx, exec.ID, node.ID, epoch, now, exp)
	if err != nil {
		return 0, false
	}
	if !ok {
		// lost a race: report the lease only if it is still live and as long
		cur, err := s.Store.GetExecution(ctx, execID)
		if err != nil || cur.EndedAt != nil || cur.Epoch != epoch || cur.LeaseExpiresAt < exp {
			return 0, false
		}
		return cur.LeaseExpiresAt, true
	}
	return exp, true
}
