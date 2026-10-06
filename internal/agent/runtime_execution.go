package agent

import (
	"context"
	"github.com/viewlegacy/onprest/internal/agent/artifacts"
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
				if c.Executor.Type == "csv" || c.Executor.Type == "file" && c.Executor.Settings["write"] == nil {
					v.EffectClass = "none"
				}
				meta.Capabilities[name] = v
			}
			if c.Output.Files != nil {
				v := meta.Capabilities[name]
				delivery := c.Output.Files["delivery"].(map[string]any)
				v.Output = &protocol.OutputMetadata{REST: delivery["rest"].(string), MCP: delivery["mcp"].(string), MaxFileBytes: c.Policy["max_output_file_bytes"].(int64), MaxTotalBytes: c.Policy["max_total_output_file_bytes"].(int64), MaxFiles: c.Policy["max_output_files"].(int64), TTL: c.Policy["file_ttl"].(string), TransferTimeout: r.versioned.Runtime()["transfer_timeout"].(string), MCPOutputSchema: c.SuccessSchema("mcp")}
				if v.Output.MCP == "inline" {
					v.Output.InlineBytes = c.Policy["mcp_inline_total_bytes"].(int64)
				}
				meta.Capabilities[name] = v
			}
			if schemaHasFile(c.Input) {
				v := meta.Capabilities[name]
				v.InputTimeout = r.versioned.Runtime()["transfer_timeout"].(string)
				v.InputLimits["max_input_file_bytes"] = l.MaxFileBytes
				v.InputLimits["max_total_input_file_bytes"] = l.MaxTotalFileBytes
				v.InputLimits["max_input_files"] = l.MaxFiles
				v.InputLimits["max_multipart_parts"] = r.versioned.Runtime()["input_limits"].(map[string]any)["max_multipart_parts"].(int64)
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
	snapshot, ok := r.versioned.Snapshot(req.Capability)
	if !ok {
		return protocol.Response{ID: req.ID, Error: inputFailure("NOT_FOUND")}
	}
	cap := snapshot.Definition()
	var scope *artifacts.OutputScope
	var session *transportSession
	var owner artifacts.Owner
	if cap.Output.Files != nil {
		session, _ = ctx.Value(outputSessionKey{}).(*transportSession)
		if session == nil || r.store == nil {
			return protocol.Response{ID: req.ID, Error: inputFailure("EXECUTION_FAILED")}
		}
		owner = artifacts.Owner{RequestID: req.ID, CallerBinding: req.CallerBinding, Capability: req.Capability, DefinitionHash: req.DefinitionHash, AuthGeneration: session.generation}
		ttl, _ := time.ParseDuration(cap.Policy["file_ttl"].(string))
		var e error
		scope, e = r.store.Output(owner, artifacts.OutputLimits{MaxFileBytes: cap.Policy["max_output_file_bytes"].(int64), MaxTotalBytes: cap.Policy["max_total_output_file_bytes"].(int64), MaxFiles: cap.Policy["max_output_files"].(int64), TTL: ttl})
		if e != nil {
			return protocol.Response{ID: req.ID, Error: inputFailure(artifactCode(e))}
		}
	}
	out, failure := r.controller.Execute(ctx, ExecutionRequest{ID: req.ID, Capability: req.Capability, DefinitionHash: req.DefinitionHash, Params: req.Params, Deadline: req.Deadline, admitted: admitted, owner: owner, stager: scope})
	if failure != nil {
		return protocol.Response{Type: "error", ID: req.ID, Error: failure}
	}
	defer out.Close()
	value := contract.ResultValue{Data: out.Data, Files: []contract.PublicFileDescriptor{}}
	b, err := contract.CanonicalJSON(value.Data)
	if err != nil {
		return protocol.Response{Type: "error", ID: req.ID, Error: &contract.ErrorValue{Code: "OUTPUT_INVALID", Message: executionMessage("OUTPUT_INVALID"), Outcome: "unknown"}}
	}
	if scope != nil {
		retained, e := scope.Retain(out.Files)
		if e != nil {
			return protocol.Response{ID: req.ID, Error: &contract.ErrorValue{Code: artifactCode(e), Message: executionMessage(artifactCode(e)), Outcome: "unknown"}}
		}
		holder := retainedOwner{owner: owner, files: map[string]artifacts.Descriptor{}}
		for _, d := range retained {
			holder.files[d.ID] = d
			holder.expires = d.ExpiresAt
			value.Files = append(value.Files, contract.PublicFileDescriptor{ID: d.ID, ArtifactMetadata: d.ArtifactMetadata, ExpiresAt: d.ExpiresAt.UTC().Format(time.RFC3339Nano)})
		}
		if len(retained) > 0 {
			session.mu.Lock()
			session.owners[req.ID] = holder
			session.mu.Unlock()
		}
	}
	resp := protocol.ResultResponse(req.ID, value)
	resp.Metrics = &protocol.Metrics{ExecutorKind: cap.Executor.Type, ReturnedRows: out.ReturnedRows, AffectedRows: out.AffectedRows, FileCount: int64(len(value.Files)), ResponseBytes: int64(len(b))}
	return resp
}

func executionDeadline(cap protocol.CapabilityMetadata) time.Time {
	d, _ := time.ParseDuration(cap.ExecutionTimeout)
	return time.Now().Add(d)
}
