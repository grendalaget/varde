package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
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
	if _, _, err := s.scopedActor(ctx, req.GroupId, "events", "member"); err != nil {
		return nil, err
	}
	limit := 200
	if req.Params.Limit != nil && *req.Params.Limit > 0 {
		limit = min(*req.Params.Limit, 500)
	}
	// With a cursor, page forward from it; without one, the newest events
	// (both oldest-first).
	var evs []store.Event
	var err error
	if req.Params.After != nil {
		evs, err = s.Store.EventsAfter(ctx, req.GroupId, *req.Params.After, limit)
	} else {
		evs, err = s.Store.LatestEvents(ctx, req.GroupId, limit)
	}
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

// sseFallbackPoll bounds event latency when no change notification arrives.
const sseFallbackPoll = 5 * time.Second

// ServeSSE implements GET /v1/groups/{id}/events/stream outside oapi-codegen.
func (s *Server) ServeSSE(w http.ResponseWriter, r *http.Request) {
	// mounted outside the ServeMux (see NewHandler), so PathValue is empty;
	// extract the id from the path directly
	groupID := r.PathValue("groupId")
	if groupID == "" {
		if rest, ok := strings.CutPrefix(r.URL.Path, "/v1/groups/"); ok {
			groupID, _, _ = strings.Cut(rest, "/")
		}
	}
	ctx := r.Context()
	if _, _, err := s.scopedActor(ctx, groupID, "events", "member"); err != nil {
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
	// A fresh connection starts at the newest event (history comes from
	// ListEvents); a reconnect resumes after Last-Event-ID.
	var after int64
	if lse := r.Header.Get("Last-Event-ID"); lse != "" {
		_, _ = fmt.Sscanf(lse, "%d", &after)
	} else {
		var err error
		if after, err = s.Store.MaxEventID(ctx, groupID); err != nil {
			writeErr(w, http.StatusInternalServerError, gen.Internal, "internal error", nil)
			return
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	// send an initial comment so proxies that hold headers until the
	// first body byte (e.g. Vite dev's compression middleware, nginx
	// buffering) flush the response headers right away
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	// Changed fires when the reconciler sees the group's fingerprint move
	// (it includes the max event id). It is per-process, so on multi-replica
	// deployments events written via another replica arrive on the fallback.
	fallback := time.NewTicker(sseFallbackPoll)
	defer fallback.Stop()

	for {
		changed := s.Recon.Changed(groupID)
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
		if len(evs) == 500 {
			continue // backlog: keep draining
		}
	wait:
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.Draining:
				return
			case <-heartbeat.C:
				_, _ = fmt.Fprintf(w, ": heartbeat\n\n")
				flusher.Flush()
			case <-changed:
				break wait
			case <-fallback.C:
				break wait
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
