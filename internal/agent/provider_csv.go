package agent

import (
	"context"
	"errors"
	"io"
	"math"
	"regexp"
	"strconv"
	"unicode/utf8"

	"github.com/viewlegacy/onprest/internal/agent/artifacts"
	"github.com/viewlegacy/onprest/internal/agent/filesystem"
	"github.com/viewlegacy/onprest/internal/agent/providers"
	"github.com/viewlegacy/onprest/internal/contract"
)

type csvProvider struct {
	cap      *contract.Capability
	root     *filesystem.Root
	selector *filesystem.Selector
}

func prepareCSVProvider(ctx context.Context, snapshot *providers.Snapshot) (providers.Provider, error) {
	p := &csvProvider{cap: snapshot.Definition()}
	read := p.cap.Executor.Settings["read"].(map[string]any)
	if name, ok := read["source"].(string); ok {
		source, _ := snapshot.Source(providers.SourceKey{Group: "folders", Name: name})
		var e error
		p.root, e = filesystem.OpenRoot(source["root"].(string))
		if e != nil {
			return nil, e
		}
		p.selector, e = filesystem.PrepareSelector(read["selector"].(map[string]any), p.cap.Input)
		if e != nil {
			return nil, errors.Join(e, p.Close())
		}
	}
	if e := ctx.Err(); e != nil {
		return nil, errors.Join(e, p.Close())
	}
	return p, nil
}
func (p *csvProvider) Close() error {
	if p.root != nil {
		return p.root.Close()
	}
	return nil
}
func csvFailure(e error) error {
	var output *contract.OutputError
	if errors.As(e, &output) {
		return e
	}
	var limit *csvLimit
	if errors.As(e, &limit) {
		return &providers.Failure{Code: "LIMIT_EXCEEDED", Cause: e}
	}
	return providerFileFailure(e)
}

// Publication keeps raw private snapshots charged to quota while the common
// publisher validates and copies the parsed rows. It adds no transaction claim.
func (p *csvProvider) ExecutePublication(ctx context.Context, in map[string]any, effects *providers.Effects, publish func(contract.ProviderResult) (*contract.PreparedOutput, error)) (out *contract.PreparedOutput, err error) {
	result, cleanup, e := p.parse(ctx, in, effects)
	defer func() {
		err = errors.Join(err, cleanup())
		if err != nil {
			if out != nil {
				_ = out.Close()
				out = nil
			}
			err = csvFailure(err)
		}
	}()
	if e != nil {
		return nil, e
	}
	return publish(result)
}
func (p *csvProvider) Execute(ctx context.Context, in map[string]any, effects *providers.Effects) (result contract.ProviderResult, err error) {
	var cleanup func() error
	result, cleanup, err = p.parse(ctx, in, effects)
	err = errors.Join(err, cleanup())
	if err != nil {
		return contract.ProviderResult{}, csvFailure(err)
	}
	return result, nil
}
func (p *csvProvider) parse(ctx context.Context, in map[string]any, effects *providers.Effects) (result contract.ProviderResult, cleanup func() error, err error) {
	effects.NoChange()
	cleanup = func() error { return nil }
	settings := p.cap.Executor.Settings["csv"].(map[string]any)
	configured := settings["columns"].(map[string]any)
	conditions := p.cap.Executor.Settings["where"].([]any)
	arguments := make([]any, len(conditions))
	for i, raw := range conditions {
		w := raw.(map[string]any)
		v, e := filePointer(in, w["input"].(string))
		if e != nil || v == nil {
			return result, cleanup, errors.New("CSV filter input is missing or null")
		}
		if w["op"] == "in" {
			vs, ok := v.([]any)
			if !ok {
				return result, cleanup, errors.New("CSV in input is not an array")
			}
			for _, item := range vs {
				if item == nil {
					return result, cleanup, errors.New("CSV in element is null")
				}
			}
		}
		arguments[i] = v
	}
	readers := []io.ReadCloser{}
	read := p.cap.Executor.Settings["read"].(map[string]any)
	if pointer, ok := read["input"].(string); ok {
		v, e := filePointer(in, pointer)
		if e != nil {
			return result, cleanup, e
		}
		values := []contract.FileValue{}
		switch x := v.(type) {
		case contract.FileValue:
			values = append(values, x)
		case []any:
			for _, raw := range x {
				file, ok := raw.(contract.FileValue)
				if !ok {
					return result, cleanup, errors.New("invalid CSV file value")
				}
				values = append(values, file)
			}
		default:
			return result, cleanup, errors.New("invalid CSV file binding")
		}
		cleanup = func() error {
			var es []error
			for _, r := range readers {
				es = append(es, r.Close())
			}
			return errors.Join(es...)
		}
		for _, v := range values {
			r, e := OpenInput(ctx, v)
			if e != nil {
				return result, cleanup, e
			}
			readers = append(readers, r)
		}
	} else {
		selected, e := p.root.Select(ctx, p.selector, in)
		if e != nil {
			return result, cleanup, e
		}
		store, _ := ctx.Value(artifactStoreKey{}).(*artifacts.Store)
		if store == nil {
			return result, cleanup, errors.New("private CSV storage is unavailable")
		}
		snapshot, e := filesystem.SnapshotFiles(ctx, selected, filesystem.SnapshotLimits{MaxFileBytes: store.Capacity(), MaxTotalBytes: store.Capacity(), MaxFiles: int64(len(selected))}, "application/octet-stream", store.Scratch())
		if e != nil {
			return result, cleanup, e
		}
		cleanup = snapshot.Close
		for _, file := range snapshot.Files {
			readers = append(readers, file.Reader)
		}
	}
	rows := []any{}
	columns := []string{}
	publish := p.cap.Output.Data || p.cap.Output.Files != nil && p.cap.Output.Files["format"] == "csv"
	items := int64(1)
	count := int64(0)
	for fileIndex, reader := range readers {
		delimiter, _ := utf8.DecodeRuneInString(settings["delimiter"].(string))
		parser := &csvRecords{chars: newCSVCharacters(ctx, reader, settings["encoding"].(string)), delimiter: delimiter}
		header := []string{}
		if settings["header"] == true {
			header, err = parser.read()
			if err == io.EOF {
				err = errors.New("CSV header is missing")
			}
			if err != nil {
				return result, cleanup, err
			}
			seen := map[string]bool{}
			for _, name := range header {
				if name == "" || seen[name] {
					return result, cleanup, errors.New("invalid or duplicate CSV header")
				}
				seen[name] = true
			}
			for name := range configured {
				if !seen[name] {
					return result, cleanup, errors.New("configured CSV column is missing")
				}
			}
		} else {
			header = make([]string, len(configured))
			for name, raw := range configured {
				header[raw.(map[string]any)["position"].(int64)] = name
			}
		}
		for _, raw := range conditions {
			name := raw.(map[string]any)["column"].(string)
			found := false
			for _, h := range header {
				if h == name {
					found = true
					break
				}
			}
			if !found {
				return result, cleanup, errors.New("CSV filter column is missing")
			}
		}
		if fileIndex == 0 {
			columns = header
		} else {
			if len(columns) != len(header) {
				return result, cleanup, errors.New("CSV headers differ")
			}
			for i := range columns {
				if columns[i] != header[i] {
					return result, cleanup, errors.New("CSV headers differ")
				}
			}
		}
		for {
			record, e := parser.read()
			if e == io.EOF {
				break
			}
			if e != nil {
				return result, cleanup, e
			}
			if len(record) != len(header) {
				return result, cleanup, errors.New("CSV field count differs from header")
			}
			row := make(map[string]any, len(header))
			for i, name := range header {
				value, e := csvCell(record[i], configured[name])
				if e != nil {
					return result, cleanup, e
				}
				row[name] = value
			}
			match := true
			for i, raw := range conditions {
				w := raw.(map[string]any)
				if !csvMatches(row[w["column"].(string)], w["op"].(string), arguments[i]) {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			if count >= p.cap.Policy["max_rows"].(int64) {
				return result, cleanup, &csvLimit{}
			}
			count++
			if publish {
				maxItems := p.cap.Policy["max_json_items"].(int64)
				rowItems := int64(1) + 2*int64(len(header))
				if rowItems > maxItems-items || p.cap.Policy["max_json_depth"].(int64) < 3 {
					return result, cleanup, &csvLimit{}
				}
				items += rowItems
				rows = append(rows, row)
			}
		}
	}
	// All headers and records have been validated before publication is invoked.
	result = contract.ProviderResult{Data: rows, Columns: columns, ReturnedRows: &count, Files: []contract.ProviderFile{}}
	return result, cleanup, nil
}

var csvInteger = regexp.MustCompile(`^[+-]?[0-9]+$`)
var csvNumber = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

func csvCell(cell string, raw any) (any, error) {
	if raw == nil {
		return cell, nil
	}
	column := raw.(map[string]any)
	for _, null := range column["null_values"].([]string) {
		if cell == null {
			if column["nullable"] == true {
				return nil, nil
			}
			return nil, errors.New("CSV null is not allowed")
		}
	}
	switch column["type"] {
	case "string":
		return cell, nil
	case "integer":
		if !csvInteger.MatchString(cell) {
			break
		}
		n, e := strconv.ParseInt(cell, 10, 64)
		if e == nil {
			return n, nil
		}
	case "number":
		if !csvNumber.MatchString(cell) {
			break
		}
		n, e := strconv.ParseFloat(cell, 64)
		if e == nil && !math.IsInf(n, 0) && !math.IsNaN(n) {
			return n, nil
		}
	case "boolean":
		if cell == "true" {
			return true, nil
		}
		if cell == "false" {
			return false, nil
		}
	}
	return nil, errors.New("CSV cell does not match its parsed type")
}
func csvMatches(value any, op string, arg any) bool {
	if value == nil {
		return false
	}
	if op == "in" {
		for _, v := range arg.([]any) {
			if csvMatches(value, "eq", v) {
				return true
			}
		}
		return false
	}
	comparison := 0
	switch x := value.(type) {
	case string:
		y, ok := arg.(string)
		if !ok {
			return false
		}
		comparison = compareCSV(x, y)
	case int64:
		y, ok := arg.(int64)
		if !ok {
			return false
		}
		comparison = compareCSV(x, y)
	case float64:
		var y float64
		switch v := arg.(type) {
		case float64:
			y = v
		case int64:
			y = float64(v)
		default:
			return false
		}
		comparison = compareCSV(x, y)
	case bool:
		y, ok := arg.(bool)
		if !ok {
			return false
		}
		if x != y {
			if x {
				comparison = 1
			} else {
				comparison = -1
			}
		}
	default:
		return false
	}
	switch op {
	case "eq":
		return comparison == 0
	case "ne":
		return comparison != 0
	case "lt":
		return comparison < 0
	case "lte":
		return comparison <= 0
	case "gt":
		return comparison > 0
	case "gte":
		return comparison >= 0
	}
	return false
}
func compareCSV[T ~string | ~int64 | ~float64](a, b T) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
