package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
)

func validateParams(cap CapabilityDef, input map[string]any) (map[string]any, error) {
	if input == nil {
		input = map[string]any{}
	}
	out := map[string]any{}
	for name := range input {
		if _, ok := cap.Params[name]; !ok {
			return nil, fmt.Errorf("unknown param: %s", name)
		}
	}
	for name, def := range cap.Params {
		v, ok := input[name]
		if !ok {
			if def.Default != nil {
				v = def.Default
				ok = true
			}
		}
		if !ok {
			if def.Required {
				return nil, fmt.Errorf("required param missing: %s", name)
			}
			continue
		}
		coerced, err := coerce(def, v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if err := validateEnum(def, coerced); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[name] = coerced
	}
	return out, nil
}

func coerce(def ParamDef, v any) (any, error) {
	switch def.Type {
	case "string":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("must be string")
		}
		if def.Pattern != "" && !regexp.MustCompile(def.Pattern).MatchString(s) {
			return nil, fmt.Errorf("does not match pattern")
		}
		if def.MinLength != nil && len(s) < *def.MinLength {
			return nil, fmt.Errorf("below minLength")
		}
		if def.MaxLength != nil && len(s) > *def.MaxLength {
			return nil, fmt.Errorf("above maxLength")
		}
		if err := validateFormat(def.Format, s); err != nil {
			return nil, err
		}
		return s, nil
	case "integer":
		i, ok := integer(v)
		if !ok {
			return nil, fmt.Errorf("must be integer")
		}
		if def.Minimum != nil && i < *def.Minimum {
			return nil, fmt.Errorf("below minimum")
		}
		if def.Maximum != nil && i > *def.Maximum {
			return nil, fmt.Errorf("above maximum")
		}
		return i, nil
	case "number":
		n, ok := number(v)
		if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, fmt.Errorf("must be number")
		}
		if def.Minimum != nil && n < float64(*def.Minimum) {
			return nil, fmt.Errorf("below minimum")
		}
		if def.Maximum != nil && n > float64(*def.Maximum) {
			return nil, fmt.Errorf("above maximum")
		}
		return n, nil
	case "boolean":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("must be boolean")
		}
		return b, nil
	default:
		return nil, fmt.Errorf("unsupported type")
	}
}

func integer(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint:
		if uint64(n) > math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		if n > math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case float32:
		return exactFloatInteger(float64(n))
	case float64:
		return exactFloatInteger(n)
	default:
		return 0, false
	}
}

func exactFloatInteger(n float64) (int64, bool) {
	const maxExactInteger = float64(1 << 53)
	if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || n > maxExactInteger || n < -maxExactInteger {
		return 0, false
	}
	return int64(n), true
}

func validateEnum(def ParamDef, value any) error {
	if len(def.Enum) == 0 {
		return nil
	}
	for _, candidate := range def.Enum {
		coerced, err := coerce(ParamDef{
			Type:      def.Type,
			Minimum:   def.Minimum,
			Maximum:   def.Maximum,
			MinLength: def.MinLength,
			MaxLength: def.MaxLength,
			Pattern:   def.Pattern,
			Format:    def.Format,
		}, candidate)
		if err == nil && coerced == value {
			return nil
		}
	}
	return fmt.Errorf("not in enum")
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	default:
		return 0, false
	}
}

func dummyParams(cap CapabilityDef) map[string]any {
	out := map[string]any{}
	for name, def := range cap.Params {
		switch def.Type {
		case "string":
			out[name] = "x"
		case "integer":
			out[name] = int64(1)
		case "number":
			out[name] = float64(1)
		case "boolean":
			out[name] = true
		}
	}
	return out
}
