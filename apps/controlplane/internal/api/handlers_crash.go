package api

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"

	"github.com/grendalaget/varde/apps/controlplane/internal/api/gen"
	"github.com/grendalaget/varde/apps/controlplane/internal/store"
	"github.com/grendalaget/varde/go/ids"
)

// Largest base64 minidump accepted in one report (~12 MiB of binary).
const maxDumpB64 = 16 * 1024 * 1024

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// AgentSubmitCrashReport stores one report from the calling node. The dump
// decodes inline; over-size bodies are rejected at the mount layer.
func (s *Server) AgentSubmitCrashReport(ctx context.Context, req gen.AgentSubmitCrashReportRequestObject) (gen.AgentSubmitCrashReportResponseObject, error) {
	node := nodeFrom(ctx)
	if node == nil {
		return nil, errResp(gen.Unauthorized, "no node", nil)
	}
	b := req.Body
	if b == nil {
		return nil, errResp(gen.Validation, "missing body", nil)
	}
	var dump *[]byte
	if d := str(b.DumpB64); d != "" {
		if len(d) > maxDumpB64 {
			return nil, errResp(gen.Validation, "dump too large", nil)
		}
		raw, err := base64.StdEncoding.DecodeString(d)
		if err != nil {
			return nil, errResp(gen.Validation, "bad dump_b64", nil)
		}
		dump = &raw
	}
	pid := int64(0)
	if b.Pid != nil {
		pid = int64(*b.Pid)
	}
	r := &store.CrashReport{
		ID:          ids.Must("cr"),
		GroupID:     node.GroupID,
		NodeID:      node.ID,
		Kind:        string(b.Kind),
		Exe:         b.Exe,
		Version:     str(b.Version),
		Os:          str(b.Os),
		Arch:        str(b.Arch),
		Pid:         pid,
		Message:     str(b.Message),
		Location:    str(b.Location),
		Backtrace:   str(b.Backtrace),
		Detail:      str(b.Detail),
		CreatedAt:   b.CreatedAtUnixMs,
		ReceivedAt:  s.Store.NowMs(),
		Dump:        dump,
		DumpName:    str(b.DumpName),
		DumpOmitted: str(b.DumpOmitted),
	}
	if err := s.Store.InsertCrashReport(ctx, r); err != nil {
		return nil, err
	}
	s.Log.Info("crash report", "node", node.ID, "kind", r.Kind, "exe", r.Exe, "msg", r.Message)
	return gen.AgentSubmitCrashReport201JSONResponse{Id: r.ID}, nil
}

// ListCrashReports: group admins see what their machines reported.
func (s *Server) ListCrashReports(ctx context.Context, req gen.ListCrashReportsRequestObject) (gen.ListCrashReportsResponseObject, error) {
	if _, _, err := s.requireRole(ctx, req.GroupId, "admin"); err != nil {
		return nil, err
	}
	limit := 100
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	rows, err := s.Store.ListCrashReports(ctx, req.GroupId, limit)
	if err != nil {
		return nil, err
	}
	out := make([]gen.CrashReportMeta, 0, len(rows))
	for _, r := range rows {
		out = append(out, gen.CrashReportMeta{
			Id:               r.ID,
			NodeId:           r.NodeID,
			NodeName:         &r.NodeName,
			Kind:             r.Kind,
			Exe:              r.Exe,
			Version:          &r.Version,
			Message:          &r.Message,
			CreatedAtUnixMs:  r.CreatedAt,
			ReceivedAtUnixMs: r.ReceivedAt,
			DumpSize:         ptrInt(int(r.DumpSize)),
			DumpOmitted:      &r.DumpOmitted,
		})
	}
	return gen.ListCrashReports200JSONResponse{Reports: out}, nil
}

// GetCrashReport returns the report incl. its stored minidump (base64).
func (s *Server) GetCrashReport(ctx context.Context, req gen.GetCrashReportRequestObject) (gen.GetCrashReportResponseObject, error) {
	if _, _, err := s.requireRole(ctx, req.GroupId, "admin"); err != nil {
		return nil, err
	}
	r, err := s.Store.GetCrashReport(ctx, req.GroupId, req.ReportId)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errResp(gen.NotFound, "crash report not found", nil)
	}
	if err != nil {
		return nil, err
	}
	var dumpB64 string
	if r.Dump != nil {
		dumpB64 = base64.StdEncoding.EncodeToString(*r.Dump)
	}
	dumpSize := 0
	if r.Dump != nil {
		dumpSize = len(*r.Dump)
	}
	pid := int(r.Pid)
	return gen.GetCrashReport200JSONResponse{
		Id:               r.ID,
		NodeId:           r.NodeID,
		NodeName:         nil,
		Kind:             r.Kind,
		Exe:              r.Exe,
		Version:          &r.Version,
		Os:               &r.Os,
		Arch:             &r.Arch,
		Pid:              &pid,
		Message:          &r.Message,
		Location:         &r.Location,
		Backtrace:        &r.Backtrace,
		Detail:           &r.Detail,
		CreatedAtUnixMs:  r.CreatedAt,
		ReceivedAtUnixMs: r.ReceivedAt,
		DumpSize:         &dumpSize,
		DumpName:         &r.DumpName,
		DumpB64:          &dumpB64,
		DumpOmitted:      &r.DumpOmitted,
	}, nil
}

func ptrInt(v int) *int { return &v }
