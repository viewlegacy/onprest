package filesystem

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/viewlegacy/onprest/internal/contract"
)

// Selector is immutable and safe to share. PrepareSelector consumes the
// compiler-normalized selector and the matching input schema, taking its own
// copy of the fields. User input cannot replace a root, path, or regexp.
type Selector struct {
	directory, name, pointer string
	expression               *regexp.Regexp
	recursive                bool
	by, order                string
	take, scanLimit          int64
}

func PrepareSelector(normalized map[string]any, input *contract.Schema) (*Selector, error) {
	s := &Selector{}
	var ok bool
	if s.directory, ok = normalized["path"].(string); !ok {
		return nil, failure("VALIDATION_FAILED", fs.ErrInvalid)
	}
	if err := allowedRelative(s.directory); err != nil {
		return nil, failure("VALIDATION_FAILED", err)
	}
	_, hasName := normalized["name"]
	_, hasRegex := normalized["name_regex"]
	if hasName == hasRegex {
		return nil, failure("VALIDATION_FAILED", fs.ErrInvalid)
	}
	if hasName {
		switch v := normalized["name"].(type) {
		case string:
			s.name = v
			if err := contract.ValidateBasename(v); err != nil {
				return nil, failure("VALIDATION_FAILED", err)
			}
		case map[string]any:
			p, ok := v["input"].(string)
			if !ok || len(v) != 1 || input == nil {
				return nil, failure("VALIDATION_FAILED", fs.ErrInvalid)
			}
			leaf, guaranteed, err := input.ResolvePointer(p)
			if err != nil || !guaranteed || len(leaf.Types) != 1 || leaf.Types[0] != "string" {
				return nil, failure("VALIDATION_FAILED", fs.ErrInvalid)
			}
			s.pointer = p
		default:
			return nil, failure("VALIDATION_FAILED", fs.ErrInvalid)
		}
	} else {
		pattern, ok := normalized["name_regex"].(string)
		if !ok || len(pattern) < 1 || len(pattern) > 4096 {
			return nil, failure("VALIDATION_FAILED", fs.ErrInvalid)
		}
		// Wrapping, rather than comparing FindString, preserves alternation and
		// rejects partial matches even when the expression itself includes anchors.
		expression, err := contract.CompileSelectorRegexp(pattern)
		if err != nil {
			return nil, failure("VALIDATION_FAILED", err)
		}
		s.expression = expression
	}
	if s.recursive, ok = normalized["recursive"].(bool); !ok {
		return nil, failure("VALIDATION_FAILED", fs.ErrInvalid)
	}
	order, ok := normalized["sort"].(map[string]any)
	if !ok {
		return nil, failure("VALIDATION_FAILED", fs.ErrInvalid)
	}
	s.by, _ = order["by"].(string)
	s.order, _ = order["order"].(string)
	if s.by != "name" && s.by != "modified_at" && s.by != "size" || s.order != "asc" && s.order != "desc" {
		return nil, failure("VALIDATION_FAILED", fs.ErrInvalid)
	}
	s.take, ok = normalized["take"].(int64)
	if !ok || s.take < 1 {
		return nil, failure("VALIDATION_FAILED", fs.ErrInvalid)
	}
	s.scanLimit, ok = normalized["scan_limit"].(int64)
	if !ok || s.scanLimit < 1 {
		return nil, failure("VALIDATION_FAILED", fs.ErrInvalid)
	}
	return s, nil
}
func pointerValue(v any, p string) (any, error) {
	if p == "" {
		return v, nil
	}
	for _, raw := range strings.Split(p[1:], "/") {
		token := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		switch x := v.(type) {
		case map[string]any:
			var ok bool
			v, ok = x[token]
			if !ok {
				return nil, fs.ErrInvalid
			}
		case []any:
			i, err := strconv.ParseInt(token, 10, 64)
			if err != nil || i < 0 || i >= int64(len(x)) {
				return nil, fs.ErrInvalid
			}
			v = x[i]
		default:
			return nil, fs.ErrInvalid
		}
	}
	return v, nil
}

// Selection contains only root-relative metadata. The private identity binds
// it to the descriptor inspected during selection; callers cannot forge a
// Selection by setting public fields. Snapshot rechecks that binding.
type Selection struct {
	RelativePath string
	Name         string
	SizeBytes    int64
	ModifiedAt   time.Time
	original     stamp
	root         *Root
	relative     string
}

func (r *Root) Select(ctx context.Context, s *Selector, params map[string]any) ([]Selection, error) {
	return r.selectFiles(ctx, s, params, nil)
}

// beforeQueuedDirectory is an optional traversal observer used to place a
// deterministic adversary barrier between parent enumeration and child open.
// The public operation uses the identical traversal without an observer.
func (r *Root) selectFiles(ctx context.Context, s *Selector, params map[string]any, beforeQueuedDirectory func(string)) ([]Selection, error) {
	if s == nil {
		return nil, failure("VALIDATION_FAILED", fs.ErrInvalid)
	}
	name := s.name
	if s.pointer != "" {
		v, err := pointerValue(params, s.pointer)
		if err != nil {
			return nil, failure("VALIDATION_FAILED", err)
		}
		var ok bool
		name, ok = v.(string)
		if !ok {
			return nil, failure("VALIDATION_FAILED", fs.ErrInvalid)
		}
	}
	if s.expression == nil {
		if err := contract.ValidateBasename(name); err != nil {
			return nil, failure("VALIDATION_FAILED", err)
		}
		if err := platformName(name); err != nil {
			return nil, failure("VALIDATION_FAILED", err)
		}
	}
	matches := []Selection{}
	type queuedDirectory struct {
		path     string
		expected stamp
		bound    bool
	}
	pending := []queuedDirectory{{path: s.directory}}
	var scanned int64
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		queued := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		directory := queued.path
		if queued.bound && beforeQueuedDirectory != nil {
			beforeQueuedDirectory(directory)
		}
		dir, before, err := r.open(directory)
		if err != nil {
			if queued.bound {
				return nil, changed(err)
			}
			return nil, err
		}
		if queued.bound && !sameStamp(queued.expected, before) {
			return nil, joinFailures(changed(errors.New("queued directory changed before scan")), dir.Close())
		}
		if !before.info.IsDir() {
			_ = dir.Close()
			return nil, failure("PERMISSION_DENIED", fs.ErrPermission)
		}
		scanErr := func() error {
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				names, err := dir.Readdirnames(1)
				if err != nil && err != io.EOF {
					return osFailure(err)
				}
				for _, base := range names {
					if scanned == s.scanLimit {
						return failure("LIMIT_EXCEEDED", errors.New("folder scan limit exceeded"))
					}
					scanned++
					if err := contract.ValidateBasename(base); err != nil {
						return failure("PERMISSION_DENIED", err)
					}
					if reserved(base) {
						continue
					}
					relative := path.Join(directory, base)
					candidate, st, e := r.open(relative)
					if e != nil {
						if errors.Is(e, fs.ErrNotExist) {
							return changed(e)
						}
						return e
					}
					if e := candidate.Close(); e != nil {
						return osFailure(e)
					}
					if st.info.IsDir() {
						if s.recursive {
							pending = append(pending, queuedDirectory{path: relative, expected: st, bound: true})
						}
						continue
					}
					if s.expression != nil && !s.expression.MatchString(base) || s.expression == nil && base != name {
						continue
					}
					matches = append(matches, Selection{RelativePath: relative, Name: base, SizeBytes: st.info.Size(), ModifiedAt: st.info.ModTime(), original: st, root: r, relative: relative})
				}
				if err == io.EOF {
					break
				}
			}
			after, e := inspect(dir)
			if e != nil {
				return changed(e)
			}
			if !sameStamp(before, after) {
				return changed(errors.New("scanned directory changed"))
			}
			current, st, e := r.open(directory)
			if e != nil {
				return changed(e)
			}
			closeErr := current.Close()
			if !sameStamp(before, st) {
				return changed(errors.New("scanned directory binding changed"))
			}
			if closeErr != nil {
				return osFailure(closeErr)
			}
			return nil
		}()
		closeErr := dir.Close()
		if scanErr != nil || closeErr != nil {
			return nil, joinFailures(scanErr, closeErr)
		}
	}
	if len(matches) == 0 {
		return nil, failure("NOT_FOUND", fs.ErrNotExist)
	}
	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		comparison := 0
		switch s.by {
		case "name":
			comparison = strings.Compare(a.Name, b.Name)
		case "size":
			if a.SizeBytes < b.SizeBytes {
				comparison = -1
			} else if a.SizeBytes > b.SizeBytes {
				comparison = 1
			}
		case "modified_at":
			if a.ModifiedAt.Before(b.ModifiedAt) {
				comparison = -1
			} else if a.ModifiedAt.After(b.ModifiedAt) {
				comparison = 1
			}
		}
		if comparison == 0 {
			return a.RelativePath < b.RelativePath
		}
		if s.order == "desc" {
			return comparison > 0
		}
		return comparison < 0
	})
	if int64(len(matches)) > s.take {
		matches = matches[:s.take]
	}
	return matches, nil
}
func (s Selection) Open(ctx context.Context) (*File, error) {
	if s.root == nil {
		return nil, failure("VALIDATION_FAILED", fs.ErrInvalid)
	}
	f, err := s.root.Open(ctx, s.relative)
	if err != nil {
		return nil, changed(err)
	}
	if !sameStamp(s.original, f.before) {
		_ = f.Close()
		return nil, changed(errors.New("selection changed before open"))
	}
	return f, nil
}
