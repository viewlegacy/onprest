package agent

import (
	"context"
	"github.com/viewlegacy/onprest/internal/contract"
	"github.com/viewlegacy/onprest/internal/protocol"
	"time"
)

func (r *Runner) executeVersioned(ctx context.Context, req protocol.Request) protocol.Response {
	return r.executeVersionedPrepared(ctx, req, false)
}
func (r *Runner) executeVersionedPrepared(ctx context.Context, req protocol.Request, admitted bool) protocol.Response {
	if req.Capability == "meta" {
		meta := protocol.Metadata{Data: contract.BuildOpenAPI(r.cf.Compiled), Capabilities: map[string]protocol.CapabilityMetadata{}, ResponseKinds: responseKinds(r.cf)}
		for _, name := range r.versioned.Names() {
			s, _ := r.versioned.Snapshot(name)
			c := s.Definition()
			effect := "possible"
			if c.Executor.Operation == "select" {
				effect = "none"
			}
			l := c.Limits()
			meta.Capabilities[name] = protocol.CapabilityMetadata{DefinitionHash: s.DefinitionHash(), ExecutorKind: c.Executor.Type, ExecutionTimeout: c.Policy["timeout"].(string), EffectClass: effect, InputSchema: c.Input.Normalized(), InputLimits: map[string]int64{"max_input_bytes": l.MaxBytes, "max_json_depth": l.MaxDepth, "max_json_items": l.MaxItems}}
			if c.Executor.Type != "db" {
				meta.ResponseKinds[name] = "generic"
				v := meta.Capabilities[name]
				if c.Executor.Type == "csv" {
					v.EffectClass = "none"
				}
				meta.Capabilities[name] = v
			}
			if schemaHasFile(c.Input) {
				v := meta.Capabilities[name]
				v.InputTimeout = r.versioned.Runtime()["transfer_timeout"].(string)
				v.InputLimits["max_input_file_bytes"] = l.MaxFileBytes
				v.InputLimits["max_total_input_file_bytes"] = l.MaxTotalFileBytes
				v.InputLimits["max_input_files"] = l.MaxFiles
				meta.Capabilities[name] = v
			}
		}
		resp := protocol.ResultResponse(req.ID, meta)
		resp.Type = "metadata"
		return resp
	}
	if req.DefinitionHash == "" {
		return protocol.Response{Type: "error", ID: req.ID, Error: &protocol.Error{Code: "INVALID_REQUEST", Message: "definition hash is required", Outcome: "not_started"}}
	}
	out, failure := r.controller.Execute(ctx, ExecutionRequest{ID: req.ID, Capability: req.Capability, DefinitionHash: req.DefinitionHash, Params: req.Params, Deadline: req.Deadline, admitted: admitted})
	if failure != nil {
		return protocol.Response{Type: "error", ID: req.ID, Error: failure}
	}
	defer out.Close()
	value := contract.ResultValue{Data: out.Data, Files: []contract.PublicFileDescriptor{}}
	b, err := contract.CanonicalJSON(value.Data)
	if err != nil {
		return protocol.Response{Type: "error", ID: req.ID, Error: &contract.ErrorValue{Code: "OUTPUT_INVALID", Message: executionMessage("OUTPUT_INVALID"), Outcome: "unknown"}}
	}
	resp := protocol.ResultResponse(req.ID, value)
	resp.Metrics = &protocol.Metrics{ExecutorKind: "db", ReturnedRows: out.ReturnedRows, AffectedRows: out.AffectedRows, ResponseBytes: int64(len(b))}
	return resp
}

func executionDeadline(cap protocol.CapabilityMetadata) time.Time {
	d, _ := time.ParseDuration(cap.ExecutionTimeout)
	return time.Now().Add(d)
}
