package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
)

const (
	directivesWaitDefaultMs = 20_000
	directivesWaitMinMs     = 1_000
	directivesWaitMaxMs     = 25_000
	directivesWaitFallback  = time.Second
	directivesWaitThrottle  = 100 * time.Millisecond
)

func directivesDigest(d gen.AgentDirectives) string {
	d.DirectivesDigest = nil
	d.ServerTimeUnixMs = 0

	if d.Executions != nil {
		executions := append([]gen.ExecutionDirective(nil), (*d.Executions)...)
		for i := range executions {
			execution := &executions[i]
			execution.LeaseExpiresAtUnixMs = 0
			if execution.LatestSafeSaveOn != nil {
				holders := append([]gen.SaveHolder(nil), (*execution.LatestSafeSaveOn)...)
				for j := range holders {
					holders[j].LastSeenAt = nil
				}
				sort.Slice(holders, func(i, j int) bool { return holders[i].NodeId < holders[j].NodeId })
				execution.LatestSafeSaveOn = &holders
			}
			if execution.LatestSaveOn != nil {
				holders := append([]gen.SaveHolder(nil), (*execution.LatestSaveOn)...)
				for j := range holders {
					holders[j].LastSeenAt = nil
				}
				sort.Slice(holders, func(i, j int) bool { return holders[i].NodeId < holders[j].NodeId })
				execution.LatestSaveOn = &holders
			}
			if execution.Restore != nil {
				restore := *execution.Restore
				restore.SourceNodeIds = append([]string(nil), restore.SourceNodeIds...)
				sort.Strings(restore.SourceNodeIds)
				execution.Restore = &restore
			}
			if execution.SnapshotRequests != nil {
				requests := append([]gen.SnapshotRequest(nil), (*execution.SnapshotRequests)...)
				sort.Slice(requests, func(i, j int) bool { return requests[i].RequestId < requests[j].RequestId })
				execution.SnapshotRequests = &requests
			}
		}
		sort.Slice(executions, func(i, j int) bool { return executions[i].ExecutionId < executions[j].ExecutionId })
		d.Executions = &executions
	}

	if d.Peers != nil {
		peers := append([]gen.DirectivePeer(nil), (*d.Peers)...)
		for i := range peers {
			if peers[i].Endpoints != nil {
				endpoints := append([]string(nil), (*peers[i].Endpoints)...)
				sort.Strings(endpoints)
				peers[i].Endpoints = &endpoints
			}
			if peers[i].RelayIds != nil {
				relayIDs := append([]string(nil), (*peers[i].RelayIds)...)
				sort.Strings(relayIDs)
				peers[i].RelayIds = &relayIDs
			}
		}
		sort.Slice(peers, func(i, j int) bool { return peers[i].NodeId < peers[j].NodeId })
		d.Peers = &peers
	}

	if d.Routes != nil {
		routes := append([]gen.DirectiveRoute(nil), (*d.Routes)...)
		for i := range routes {
			routes[i].DrainUntilUnixMs = nil
		}
		sort.Slice(routes, func(i, j int) bool { return routes[i].ServiceId < routes[j].ServiceId })
		d.Routes = &routes
	}

	if d.Relays != nil {
		relays := append([]gen.DirectiveRelay(nil), (*d.Relays)...)
		for i := range relays {
			relays[i].Token = ""
		}
		sort.Slice(relays, func(i, j int) bool { return relays[i].RelayId < relays[j].RelayId })
		d.Relays = &relays
	}

	if d.ReplicationTasks != nil {
		tasks := append([]gen.ReplicationTask(nil), (*d.ReplicationTasks)...)
		for i := range tasks {
			tasks[i].SourceNodeIds = append([]string(nil), tasks[i].SourceNodeIds...)
			sort.Strings(tasks[i].SourceNodeIds)
		}
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].SnapshotId < tasks[j].SnapshotId })
		d.ReplicationTasks = &tasks
	}

	if d.DeleteSnapshots != nil {
		deletions := append([]string(nil), (*d.DeleteSnapshots)...)
		sort.Strings(deletions)
		d.DeleteSnapshots = &deletions
	}

	encoded, _ := json.Marshal(d)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:16])
}

func (s *Server) AgentDirectivesWait(ctx context.Context, req gen.AgentDirectivesWaitRequestObject) (gen.AgentDirectivesWaitResponseObject, error) {
	node := nodeFrom(ctx)
	if node == nil {
		return nil, errResp(gen.Unauthorized, "no node", nil)
	}

	timeoutMs := directivesWaitDefaultMs
	if req.Params.TimeoutMs != nil {
		timeoutMs = *req.Params.TimeoutMs
	}
	if timeoutMs < directivesWaitMinMs {
		timeoutMs = directivesWaitMinMs
	} else if timeoutMs > directivesWaitMaxMs {
		timeoutMs = directivesWaitMaxMs
	}

	timeout := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
	defer timeout.Stop()
	fallback := time.NewTicker(directivesWaitFallback)
	defer fallback.Stop()
	lastCheck := time.Time{}

	for {
		changed := s.Recon.Changed()
		directives, err := s.buildDirectives(ctx, node, s.Store.NowMs())
		if err != nil {
			return nil, err
		}
		digest := directivesDigest(directives)
		if digest != req.Params.Digest {
			return gen.AgentDirectivesWait200JSONResponse(gen.AgentDirectivesWait{
				Digest: digest, Changed: true,
			}), nil
		}
		lastCheck = time.Now()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout.C:
			directives, err := s.buildDirectives(ctx, node, s.Store.NowMs())
			if err != nil {
				return nil, err
			}
			digest = directivesDigest(directives)
			return gen.AgentDirectivesWait200JSONResponse(gen.AgentDirectivesWait{
				Digest: digest, Changed: digest != req.Params.Digest,
			}), nil
		case <-changed:
		case <-fallback.C:
		}

		if delay := time.Until(lastCheck.Add(directivesWaitThrottle)); delay > 0 {
			throttle := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				throttle.Stop()
				return nil, ctx.Err()
			case <-timeout.C:
				throttle.Stop()
				directives, err := s.buildDirectives(ctx, node, s.Store.NowMs())
				if err != nil {
					return nil, err
				}
				digest = directivesDigest(directives)
				return gen.AgentDirectivesWait200JSONResponse(gen.AgentDirectivesWait{
					Digest: digest, Changed: digest != req.Params.Digest,
				}), nil
			case <-throttle.C:
			}
		}
	}
}
