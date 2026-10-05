package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/grendalaget/varde/apps/control-plane/internal/api/gen"
	"github.com/grendalaget/varde/apps/control-plane/internal/store"
)

func genEvent(e *store.Event) gen.Event {
	var data map[string]any
	_ = json.Unmarshal([]byte(e.DataJSON), &data)
	return gen.Event{
		Id: e.ID, GroupId: e.GroupID, ServerId: e.ServerID, NodeId: e.NodeID,
		Type: e.Type, Data: &data, CreatedAt: e.CreatedAt,
	}
}

func (s *Server) ListEvents(ctx context.Context, req gen.ListEventsRequestObject) (gen.ListEventsResponseObject, error) {
	if _, _, err := s.requireRole(ctx, req.GroupId, "member"); err != nil {
		return nil, err
	}
	after := int64(0)
	if req.Params.After != nil {
		after = *req.Params.After
	}
	limit := 200
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	evs, err := s.Store.EventsAfter(ctx, req.GroupId, after, limit)
	if err != nil {
		return nil, err
	}
	out := []gen.Event{}
	for i := range evs {
		out = append(out, genEvent(&evs[i]))
	}
	return gen.ListEvents200JSONResponse{Events: out}, nil
}

// StreamEvents is SSE; strict-server can't stream, so it's mounted as a raw
// handler next to the generated mux (see Mount).
func (s *Server) StreamEvents(ctx context.Context, req gen.StreamEventsRequestObject) (gen.StreamEventsResponseObject, error) {
	return nil, fmt.Errorf("handled by raw SSE handler")
}

// ServeSSE implements GET /v1/groups/{id}/events/stream outside oapi-codegen.
func (s *Server) ServeSSE(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("groupId")
	ctx := r.Context()
	if _, _, err := s.requireRole(ctx, groupID, "member"); err != nil {
		if ae, ok := err.(*apiError); ok {
			writeErr(w, statusFor(ae.code), ae.code, ae.msg, nil)
			return
		}
		writeErr(w, http.StatusForbidden, gen.Forbidden, "forbidden", nil)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, gen.Internal, "no streaming", nil)
		return
	}
	after := int64(0)
	if lse := r.Header.Get("Last-Event-ID"); lse != "" {
		_, _ = fmt.Sscanf(lse, "%d", &after)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			_, _ = fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		case <-poll.C:
			evs, err := s.Store.EventsAfter(ctx, groupID, after, 500)
			if err != nil {
				return
			}
			for i := range evs {
				e := &evs[i]
				payload, _ := json.Marshal(genEvent(e))
				_, _ = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.ID, payload)
				after = e.ID
			}
			if len(evs) > 0 {
				flusher.Flush()
			}
		}
	}
}

func (s *Server) AdminListGroups(ctx context.Context, _ gen.AdminListGroupsRequestObject) (gen.AdminListGroupsResponseObject, error) {
	if _, err := s.requireOperator(ctx); err != nil {
		return nil, err
	}
	gs, err := s.Store.AllGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := []gen.Group{}
	for i := range gs {
		out = append(out, genGroup(&gs[i]))
	}
	return gen.AdminListGroups200JSONResponse{Groups: out}, nil
}

func (s *Server) AdminSetGroupLimits(ctx context.Context, req gen.AdminSetGroupLimitsRequestObject) (gen.AdminSetGroupLimitsResponseObject, error) {
	u, err := s.requireOperator(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errResp(gen.Validation, "missing body", nil)
	}
	l := &store.GroupLimits{
		GroupID:          req.GroupId,
		MaxNodes:         intToI64Ptr(req.Body.MaxNodes),
		MaxServers:       intToI64Ptr(req.Body.MaxServers),
		MaxStorageBytes:  req.Body.MaxStorageBytes,
		MaxRelayBytesMon: req.Body.MaxRelayBytesMonth,
	}
	if err := s.Store.SetLimits(ctx, l); err != nil {
		return nil, err
	}
	_ = s.Store.Audit(ctx, s.Store.DB, &req.GroupId, "user", u.ID, "admin.limits", req.GroupId, req.Body)
	return gen.AdminSetGroupLimits200JSONResponse{
		MaxNodes: req.Body.MaxNodes, MaxServers: req.Body.MaxServers,
		MaxStorageBytes: req.Body.MaxStorageBytes, MaxRelayBytesMonth: req.Body.MaxRelayBytesMonth,
	}, nil
}

func intToI64Ptr(p *int) *int64 {
	if p == nil {
		return nil
	}
	v := int64(*p)
	return &v
}
