// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

//go:generate go run github.com/DataDog/datadog-agent/pkg/security/secl/compiler/generators/operators -output eval_operators.go

package eval

import (
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/alecthomas/participle/lexer"
	"github.com/pkg/errors"

	"github.com/DataDog/datadog-agent/pkg/security/secl/compiler/ast"
)

// defines factor applied by specific operator
const (
	FunctionWeight       = 5
	InArrayWeight        = 10
	HandlerWeight        = 50
	RegexpWeight         = 100
	InPatternArrayWeight = 1000
	IteratorWeight       = 2000
)

// BoolEvalFnc describe a eval function return a boolean
type BoolEvalFnc = func(ctx *Context) bool

func extractField(field string, state *State) (Field, Field, RegisterID, error) {
	if state.regexpCache.arraySubscriptFindRE == nil {
		state.regexpCache.arraySubscriptFindRE = regexp.MustCompile(`\[([^\]]*)\]`)
	}
	if state.regexpCache.arraySubscriptReplaceRE == nil {
		state.regexpCache.arraySubscriptReplaceRE = regexp.MustCompile(`(.+)\[[^\]]+\](.*)`)
	}

	var regID RegisterID
	ids := state.regexpCache.arraySubscriptFindRE.FindStringSubmatch(field)

	switch len(ids) {
	case 0:
		return field, "", "", nil
	case 2:
		regID = ids[1]
	default:
		return "", "", "", fmt.Errorf("wrong register format for fields: %s", field)
	}

	resField := state.regexpCache.arraySubscriptReplaceRE.ReplaceAllString(field, `$1$2`)
	itField := state.regexpCache.arraySubscriptReplaceRE.ReplaceAllString(field, `$1`)

	return resField, itField, regID, nil
}

type ident struct {
	Pos   lexer.Position
	Ident *string
}

type EvalReplacementContext struct {
	*Opts
	*MacroStore
}

func identToEvaluator(obj *ident, replCtx EvalReplacementContext, state *State) (interface{}, lexer.Position, error) {
	if accessor, ok := replCtx.Opts.Constants[*obj.Ident]; ok {
		return accessor, obj.Pos, nil
	}

	if state.macros != nil {
		if macro, ok := state.macros[*obj.Ident]; ok {
			return macro.Value, obj.Pos, nil
		}
	}

	field, itField, regID, err := extractField(*obj.Ident, state)
	if err != nil {
		return nil, obj.Pos, err
	}

	// transform extracted field to support legacy SECL fields
	if replCtx.Opts.LegacyFields != nil {
		if newField, ok := replCtx.Opts.LegacyFields[field]; ok {
			field = newField
		}
		if newField, ok := replCtx.Opts.LegacyFields[field]; ok {
			itField = newField
		}
	}

	// extract iterator
	var iterator Iterator
	if itField != "" {
		if iterator, err = state.model.GetIterator(itField); err != nil {
			return nil, obj.Pos, err
		}
	} else {
		// detect whether a iterator is along the path
		var candidate string
		for _, node := range strings.Split(field, ".") {
			if candidate == "" {
				candidate = node
			} else {
				candidate = candidate + "." + node
			}

			iterator, err = state.model.GetIterator(candidate)
			if err == nil {
				break
			}
		}
	}

	if iterator != nil {
		// Force "_" register for now.
		if regID != "" && regID != "_" {
			return nil, obj.Pos, NewRegisterNameNotAllowed(obj.Pos, regID, errors.New("only `_` is supported"))
		}

		// regID not specified or `_` generate one
		if regID == "" || regID == "_" {
			regID = RandString(8)
		}

		if info, exists := state.registersInfo[regID]; exists {
			if info.field != itField {
				return nil, obj.Pos, NewRegisterMultipleFields(obj.Pos, regID, errors.New("used by multiple fields"))
			}

			info.subFields[field] = true
		} else {
			info = &registerInfo{
				field:    itField,
				iterator: iterator,
				subFields: map[Field]bool{
					field: true,
				},
			}
			state.registersInfo[regID] = info
		}
	}

	accessor, err := state.model.GetEvaluator(field, regID)
	if err != nil {
		return nil, obj.Pos, err
	}

	state.UpdateFields(field)

	return accessor, obj.Pos, nil
}

func arrayToEvaluator(array *ast.Array, replCtx EvalReplacementContext, state *State) (interface{}, lexer.Position, error) {
	if len(array.Numbers) != 0 {
		var evaluator IntArrayEvaluator
		evaluator.AppendValues(array.Numbers...)
		return &evaluator, array.Pos, nil
	} else if len(array.StringMembers) != 0 {
		var evaluator StringValuesEvaluator
		evaluator.AppendMembers(array.StringMembers...)
		return &evaluator, array.Pos, nil
	} else if array.Ident != nil {
		if state.macros != nil {
			if macro, ok := state.macros[*array.Ident]; ok {
				return macro.Value, array.Pos, nil
			}
		}

		// could be an iterator
		return identToEvaluator(&ident{Pos: array.Pos, Ident: array.Ident}, replCtx, state)
	} else if array.Variable != nil {
		varName, ok := isVariableName(*array.Variable)
		if !ok {
			return nil, array.Pos, NewError(array.Pos, fmt.Sprintf("invalid variable name '%s'", *array.Variable))
		}
		return evaluatorFromVariable(varName, array.Pos, replCtx)
	} else if array.CIDR != nil {
		var values CIDRValues
		if err := values.AppendCIDR(*array.CIDR); err != nil {
			return nil, array.Pos, NewError(array.Pos, fmt.Sprintf("invalid CIDR '%s'", *array.CIDR))
		}

		evaluator := &CIDRValuesEvaluator{
			Value:     values,
			ValueType: IPNetValueType,
		}
		return evaluator, array.Pos, nil
	} else if len(array.CIDRMembers) != 0 {
		var values CIDRValues
		for _, member := range array.CIDRMembers {
			if member.CIDR != nil {
				if err := values.AppendCIDR(*member.CIDR); err != nil {
					return nil, array.Pos, NewError(array.Pos, fmt.Sprintf("invalid CIDR '%s'", *member.CIDR))
				}
			} else if member.IP != nil {
				if err := values.AppendIP(*member.IP); err != nil {
					return nil, array.Pos, NewError(array.Pos, fmt.Sprintf("invalid IP '%s'", *member.IP))
				}
			}
		}

		evaluator := &CIDRValuesEvaluator{
			Value:     values,
			ValueType: IPNetValueType,
		}
		return evaluator, array.Pos, nil
	}

	return nil, array.Pos, NewError(array.Pos, "unknown array element type")
}

func isVariableName(str string) (string, bool) {
	if strings.HasPrefix(str, "${") && strings.HasSuffix(str, "}") {
		return str[2 : len(str)-1], true
	}
	return "", false
}

func evaluatorFromVariable(varname string, pos lexer.Position, replCtx EvalReplacementContext) (interface{}, lexer.Position, error) {
	value, exists := replCtx.Opts.Variables[varname]
	if !exists {
		return nil, pos, NewError(pos, fmt.Sprintf("variable '%s' doesn't exist", varname))
	}

	return value.GetEvaluator(), pos, nil
}

func stringEvaluatorFromVariable(str string, pos lexer.Position, replCtx EvalReplacementContext) (interface{}, lexer.Position, error) {
	var evaluators []*StringEvaluator

	doLoc := func(sub string) error {
		if varname, ok := isVariableName(sub); ok {
			value, exists := replCtx.Opts.Variables[varname]
			if !exists {
				return NewError(pos, fmt.Sprintf("variable '%s' doesn't exist", varname))
			}

			evaluator := value.GetEvaluator()
			switch evaluator := evaluator.(type) {
			case *StringArrayEvaluator:
				evaluators = append(evaluators, &StringEvaluator{
					EvalFnc: func(ctx *Context) string {
						return strings.Join(evaluator.EvalFnc(ctx), ",")
					}})
			case *IntArrayEvaluator:
				evaluators = append(evaluators, &StringEvaluator{
					EvalFnc: func(ctx *Context) string {
						var result string
						for i, number := range evaluator.EvalFnc(ctx) {
							if i != 0 {
								result += ","
							}
							result += strconv.FormatInt(int64(number), 10)
						}
						return result
					}})
			case *StringEvaluator:
				evaluators = append(evaluators, evaluator)
			case *IntEvaluator:
				evaluators = append(evaluators, &StringEvaluator{
					EvalFnc: func(ctx *Context) string {
						return strconv.FormatInt(int64(evaluator.EvalFnc(ctx)), 10)
					}})
			default:
				return NewError(pos, fmt.Sprintf("variable type not supported '%s'", varname))
			}
		} else {
			evaluators = append(evaluators, &StringEvaluator{Value: sub})
		}

		return nil
	}

	var last int
	for _, loc := range variableRegex.FindAllIndex([]byte(str), -1) {
		if loc[0] > 0 {
			if err := doLoc(str[last:loc[0]]); err != nil {
				return nil, pos, err
			}
		}
		if err := doLoc(str[loc[0]:loc[1]]); err != nil {
			return nil, pos, err
		}
		last = loc[1]
	}
	if last < len(str) {
		if err := doLoc(str[last:]); err != nil {
			return nil, pos, err
		}
	}

	return &StringEvaluator{
		ValueType: VariableValueType,
		EvalFnc: func(ctx *Context) string {
			var result string
			for _, evaluator := range evaluators {
				if evaluator.EvalFnc != nil {
					result += evaluator.EvalFnc(ctx)
				} else {
					result += evaluator.Value
				}
			}
			return result
		},
	}, pos, nil
}

// StringEqualsWrapper makes use of operator overrides
func StringEqualsWrapper(a *StringEvaluator, b *StringEvaluator, replCtx EvalReplacementContext, state *State) (*BoolEvaluator, error) {
	var evaluator *BoolEvaluator
	var err error

	if a.OpOverrides != nil && a.OpOverrides.StringEquals != nil {
		evaluator, err = a.OpOverrides.StringEquals(a, b, replCtx, state)
	} else if b.OpOverrides != nil && b.OpOverrides.StringEquals != nil {
		evaluator, err = b.OpOverrides.StringEquals(a, b, replCtx, state)
	} else {
		evaluator, err = StringEquals(a, b, replCtx, state)
	}
	if err != nil {
		return nil, err
	}

	return evaluator, nil
}

// StringArrayContainsWrapper makes use of operator overrides
func StringArrayContainsWrapper(a *StringEvaluator, b *StringArrayEvaluator, replCtx EvalReplacementContext, state *State) (*BoolEvaluator, error) {
	var evaluator *BoolEvaluator
	var err error

	if a.OpOverrides != nil && a.OpOverrides.StringArrayContains != nil {
		evaluator, err = a.OpOverrides.StringArrayContains(a, b, replCtx, state)
	} else if b.OpOverrides != nil && b.OpOverrides.StringArrayContains != nil {
		evaluator, err = b.OpOverrides.StringArrayContains(a, b, replCtx, state)
	} else {
		evaluator, err = StringArrayContains(a, b, replCtx, state)
	}
	if err != nil {
		return nil, err
	}

	return evaluator, nil
}

// StringValuesContainsWrapper makes use of operator overrides
func StringValuesContainsWrapper(a *StringEvaluator, b *StringValuesEvaluator, replCtx EvalReplacementContext, state *State) (*BoolEvaluator, error) {
	var evaluator *BoolEvaluator
	var err error

	if a.OpOverrides != nil && a.OpOverrides.StringValuesContains != nil {
		evaluator, err = a.OpOverrides.StringValuesContains(a, b, replCtx, state)
	} else {
		evaluator, err = StringValuesContains(a, b, replCtx, state)
	}
	if err != nil {
		return nil, err
	}

	return evaluator, nil
}

// StringArrayMatchesWrapper makes use of operator overrides
func StringArrayMatchesWrapper(a *StringArrayEvaluator, b *StringValuesEvaluator, replCtx EvalReplacementContext, state *State) (*BoolEvaluator, error) {
	var evaluator *BoolEvaluator
	var err error

	if a.OpOverrides != nil && a.OpOverrides.StringArrayMatches != nil {
		evaluator, err = a.OpOverrides.StringArrayMatches(a, b, replCtx, state)
	} else {
		evaluator, err = StringArrayMatches(a, b, replCtx, state)
	}
	if err != nil {
		return nil, err
	}

	return evaluator, nil
}

func nodeToEvaluator(obj interface{}, replCtx EvalReplacementContext, state *State) (interface{}, lexer.Position, error) {
	var err error
	var boolEvaluator *BoolEvaluator
	var pos lexer.Position
	var cmp, unary, next interface{}

	switch obj := obj.(type) {
	case *ast.BooleanExpression:
		return nodeToEvaluator(obj.Expression, replCtx, state)
	case *ast.Expression:
		cmp, pos, err = nodeToEvaluator(obj.Comparison, replCtx, state)
		if err != nil {
			return nil, pos, err
		}

		if obj.Op != nil {
			cmpBool, ok := cmp.(*BoolEvaluator)
			if !ok {
				return nil, obj.Pos, NewTypeError(obj.Pos, reflect.Bool)
			}

			next, pos, err = nodeToEvaluator(obj.Next, replCtx, state)
			if err != nil {
				return nil, pos, err
			}

			nextBool, ok := next.(*BoolEvaluator)
			if !ok {
				return nil, pos, NewTypeError(pos, reflect.Bool)
			}

			switch *obj.Op {
			case "||", "or":
				boolEvaluator, err = Or(cmpBool, nextBool, replCtx, state)
				if err != nil {
					return nil, obj.Pos, err
				}
				return boolEvaluator, obj.Pos, nil
			case "&&", "and":
				boolEvaluator, err = And(cmpBool, nextBool, replCtx, state)
				if err != nil {
					return nil, obj.Pos, err
				}
				return boolEvaluator, obj.Pos, nil
			}
			return nil, pos, NewOpUnknownError(obj.Pos, *obj.Op)
		}
		return cmp, obj.Pos, nil
	case *ast.BitOperation:
		unary, pos, err = nodeToEvaluator(obj.Unary, replCtx, state)
		if err != nil {
			return nil, pos, err
		}

		if obj.Op != nil {
			bitInt, ok := unary.(*IntEvaluator)
			if !ok {
				return nil, obj.Pos, NewTypeError(obj.Pos, reflect.Int)
			}

			next, pos, err = nodeToEvaluator(obj.Next, replCtx, state)
			if err != nil {
				return nil, pos, err
			}

			nextInt, ok := next.(*IntEvaluator)
			if !ok {
				return nil, pos, NewTypeError(pos, reflect.Int)
			}

			switch *obj.Op {
			case "&":
				intEvaluator, err := IntAnd(bitInt, nextInt, replCtx, state)
				if err != nil {
					return nil, pos, err
				}
				return intEvaluator, obj.Pos, nil
			case "|":
				IntEvaluator, err := IntOr(bitInt, nextInt, replCtx, state)
				if err != nil {
					return nil, pos, err
				}
				return IntEvaluator, obj.Pos, nil
			case "^":
				IntEvaluator, err := IntXor(bitInt, nextInt, replCtx, state)
				if err != nil {
					return nil, pos, err
				}
				return IntEvaluator, obj.Pos, nil
			}
			return nil, pos, NewOpUnknownError(obj.Pos, *obj.Op)
		}
		return unary, obj.Pos, nil

	case *ast.Comparison:
		unary, pos, err = nodeToEvaluator(obj.BitOperation, replCtx, state)
		if err != nil {
			return nil, pos, err
		}

		if obj.ArrayComparison != nil {
			next, pos, err = nodeToEvaluator(obj.ArrayComparison, replCtx, state)
			if err != nil {
				return nil, pos, err
			}

			switch unary := unary.(type) {
			case *BoolEvaluator:
				switch nextBool := next.(type) {
				case *BoolArrayEvaluator:
					boolEvaluator, err = ArrayBoolContains(unary, nextBool, replCtx, state)
					if err != nil {
						return nil, pos, err
					}
					if *obj.ArrayComparison.Op == "notin" {
						return Not(boolEvaluator, replCtx, state), obj.Pos, nil
					}
					return boolEvaluator, obj.Pos, nil
				default:
					return nil, pos, NewArrayTypeError(pos, reflect.Array, reflect.Bool)
				}
			case *StringEvaluator:
				switch nextString := next.(type) {
				case *StringArrayEvaluator:
					boolEvaluator, err = StringArrayContainsWrapper(unary, nextString, replCtx, state)
					if err != nil {
						return nil, pos, err
					}
				case *StringValuesEvaluator:
					boolEvaluator, err = StringValuesContainsWrapper(unary, nextString, replCtx, state)
					if err != nil {
						return nil, pos, err
					}
				default:
					return nil, pos, NewArrayTypeError(pos, reflect.Array, reflect.String)
				}
				if *obj.ArrayComparison.Op == "notin" {
					return Not(boolEvaluator, replCtx, state), obj.Pos, nil
				}
				return boolEvaluator, obj.Pos, nil
			case *StringValuesEvaluator:
				switch nextStringArray := next.(type) {
				case *StringArrayEvaluator:
					boolEvaluator, err = StringArrayMatchesWrapper(nextStringArray, unary, replCtx, state)
					if err != nil {
						return nil, pos, err
					}
					if *obj.ArrayComparison.Op == "notin" {
						return Not(boolEvaluator, replCtx, state), obj.Pos, nil
					}
					return boolEvaluator, obj.Pos, nil
				default:
					return nil, pos, NewArrayTypeError(pos, reflect.Array, reflect.String)
				}
			case *StringArrayEvaluator:
				switch nextStringArray := next.(type) {
				case *StringValuesEvaluator:
					boolEvaluator, err = StringArrayMatchesWrapper(unary, nextStringArray, replCtx, state)
					if err != nil {
						return nil, pos, err
					}
					if *obj.ArrayComparison.Op == "notin" {
						return Not(boolEvaluator, replCtx, state), obj.Pos, nil
					}
					return boolEvaluator, obj.Pos, nil
				default:
					return nil, pos, NewArrayTypeError(pos, reflect.Array, reflect.String)
				}
			case *IntEvaluator:
				switch nextInt := next.(type) {
				case *IntArrayEvaluator:
					boolEvaluator, err = IntArrayEquals(unary, nextInt, replCtx, state)
					if err != nil {
						return nil, pos, err
					}
					if *obj.ArrayComparison.Op == "notin" {
						return Not(boolEvaluator, replCtx, state), obj.Pos, nil
					}
					return boolEvaluator, obj.Pos, nil
				default:
					return nil, pos, NewArrayTypeError(pos, reflect.Array, reflect.Int)
				}
			case *IntArrayEvaluator:
				switch nextIntArray := next.(type) {
				case *IntArrayEvaluator:
					boolEvaluator, err = IntArrayMatches(unary, nextIntArray, replCtx, state)
					if err != nil {
						return nil, pos, err
					}
					if *obj.ArrayComparison.Op == "notin" {
						return Not(boolEvaluator, replCtx, state), obj.Pos, nil
					}
					return boolEvaluator, obj.Pos, nil
				default:
					return nil, pos, NewArrayTypeError(pos, reflect.Array, reflect.Int)
				}
			case *CIDREvaluator:
				switch nextCIDR := next.(type) {
				case *CIDREvaluator:
					nextIP, ok := next.(*CIDREvaluator)
					if !ok {
						return nil, pos, NewTypeError(pos, reflect.TypeOf(CIDREvaluator{}).Kind())
					}

					boolEvaluator, err = CIDREquals(unary, nextIP, replCtx, state)
					if err != nil {
						return nil, obj.Pos, err
					}
					switch *obj.ArrayComparison.Op {
					case "in", "allin":
						return boolEvaluator, obj.Pos, nil
					case "notin":
						return Not(boolEvaluator, replCtx, state), obj.Pos, nil
					}
					return nil, pos, NewOpUnknownError(obj.Pos, *obj.ArrayComparison.Op)
				case *CIDRValuesEvaluator:
					switch *obj.ArrayComparison.Op {
					case "in", "allin":
						boolEvaluator, err = CIDRValuesContains(unary, nextCIDR, replCtx, state)
						if err != nil {
							return nil, pos, err
						}
						return boolEvaluator, obj.Pos, nil
					case "notin":
						boolEvaluator, err = CIDRValuesContains(unary, nextCIDR, replCtx, state)
						if err != nil {
							return nil, pos, err
						}
						return Not(boolEvaluator, replCtx, state), obj.Pos, nil
					}
					return nil, pos, NewOpUnknownError(obj.Pos, *obj.ArrayComparison.Op)
				case *CIDRArrayEvaluator:
					switch *obj.ArrayComparison.Op {
					case "in", "allin":
						boolEvaluator, err = CIDRArrayContains(unary, nextCIDR, replCtx, state)
						if err != nil {
							return nil, pos, err
						}
						return boolEvaluator, obj.Pos, nil
					case "notin":
						boolEvaluator, err = CIDRArrayContains(unary, nextCIDR, replCtx, state)
						if err != nil {
							return nil, pos, err
						}
						return Not(boolEvaluator, replCtx, state), obj.Pos, nil
					}
					return nil, pos, NewOpUnknownError(obj.Pos, *obj.ArrayComparison.Op)
				default:
					return nil, pos, NewCIDRTypeError(pos, reflect.Array, next)
				}
			case *CIDRArrayEvaluator:
				switch nextCIDR := next.(type) {
				case *CIDRValuesEvaluator:
					switch *obj.ArrayComparison.Op {
					case "in":
						boolEvaluator, err = CIDRArrayMatches(unary, nextCIDR, replCtx, state)
						if err != nil {
							return nil, pos, err
						}
						return boolEvaluator, obj.Pos, nil
					case "allin":
						boolEvaluator, err = CIDRArrayMatchesAll(unary, nextCIDR, replCtx, state)
						if err != nil {
							return nil, pos, err
						}
						return boolEvaluator, obj.Pos, nil
					case "notin":
						boolEvaluator, err = CIDRArrayMatches(unary, nextCIDR, replCtx, state)
						if err != nil {
							return nil, pos, err
						}
						return Not(boolEvaluator, replCtx, state), obj.Pos, nil
					}
					return nil, pos, NewOpUnknownError(obj.Pos, *obj.ArrayComparison.Op)
				default:
					return nil, pos, NewCIDRTypeError(pos, reflect.Array, next)
				}
			case *CIDRValuesEvaluator:
				switch nextCIDR := next.(type) {
				case *CIDREvaluator:
					switch *obj.ArrayComparison.Op {
					case "in", "allin":
						boolEvaluator, err = CIDRValuesContains(nextCIDR, unary, replCtx, state)
						if err != nil {
							return nil, obj.Pos, err
						}
						return boolEvaluator, obj.Pos, nil
					case "notin":
						boolEvaluator, err = CIDRValuesContains(nextCIDR, unary, replCtx, state)
						if err != nil {
							return nil, obj.Pos, err
						}
						return Not(boolEvaluator, replCtx, state), obj.Pos, nil
					}
					return nil, pos, NewOpUnknownError(obj.Pos, *obj.ArrayComparison.Op)
				case *CIDRArrayEvaluator:
					switch *obj.ArrayComparison.Op {
					case "allin":
						boolEvaluator, err = CIDRArrayMatchesAll(nextCIDR, unary, replCtx, state)
						if err != nil {
							return nil, obj.Pos, err
						}
						return boolEvaluator, obj.Pos, nil
					case "in":
						boolEvaluator, err = CIDRArrayMatches(nextCIDR, unary, replCtx, state)
						if err != nil {
							return nil, pos, err
						}
						return boolEvaluator, obj.Pos, nil
					case "notin":
						boolEvaluator, err = CIDRArrayMatches(nextCIDR, unary, replCtx, state)
						if err != nil {
							return nil, pos, err
						}
						return Not(boolEvaluator, replCtx, state), obj.Pos, nil
					}
					return nil, pos, NewOpUnknownError(obj.Pos, *obj.ArrayComparison.Op)
				default:
					return nil, pos, NewCIDRTypeError(pos, reflect.Array, next)
				}
			default:
				return nil, pos, NewTypeError(pos, reflect.Array)
			}
		} else if obj.ScalarComparison != nil {
			next, pos, err = nodeToEvaluator(obj.ScalarComparison, replCtx, state)
			if err != nil {
				return nil, pos, err
			}

			switch unary := unary.(type) {
			case *BoolEvaluator:
				nextBool, ok := next.(*BoolEvaluator)
				if !ok {
					return nil, pos, NewTypeError(pos, reflect.Bool)
				}

				switch *obj.ScalarComparison.Op {
				case "!=":
					boolEvaluator, err = BoolEquals(unary, nextBool, replCtx, state)
					if err != nil {
						return nil, pos, err
					}
					return Not(boolEvaluator, replCtx, state), obj.Pos, nil
				case "==":
					boolEvaluator, err = BoolEquals(unary, nextBool, replCtx, state)
					if err != nil {
						return nil, pos, err
					}
					return boolEvaluator, obj.Pos, nil
				}
				return nil, pos, NewOpUnknownError(obj.Pos, *obj.ScalarComparison.Op)
			case *BoolArrayEvaluator:
				nextBool, ok := next.(*BoolEvaluator)
				if !ok {
					return nil, pos, NewTypeError(pos, reflect.Bool)
				}

				switch *obj.ScalarComparison.Op {
				case "!=":
					boolEvaluator, err = BoolArrayEquals(nextBool, unary, replCtx, state)
					if err != nil {
						return nil, pos, err
					}
					return Not(boolEvaluator, replCtx, state), obj.Pos, nil
				case "==":
					boolEvaluator, err = BoolArrayEquals(nextBool, unary, replCtx, state)
					if err != nil {
						return nil, pos, err
					}
					return boolEvaluator, obj.Pos, nil
				}
				return nil, pos, NewOpUnknownError(obj.Pos, *obj.ScalarComparison.Op)
			case *StringEvaluator:
				nextString, ok := next.(*StringEvaluator)
				if !ok {
					return nil, pos, NewTypeError(pos, reflect.String)
				}

				switch *obj.ScalarComparison.Op {
				case "!=":
					boolEvaluator, err = StringEqualsWrapper(unary, nextString, replCtx, state)
					if err != nil {
						return nil, obj.Pos, err
					}
					return Not(boolEvaluator, replCtx, state), obj.Pos, nil
				case "!~":
					if nextString.EvalFnc != nil {
						return nil, obj.Pos, &ErrNonStaticPattern{Field: nextString.Field}
					}

					// force pattern if needed
					if nextString.ValueType == ScalarValueType {
						nextString.ValueType = PatternValueType
					}

					boolEvaluator, err = StringEqualsWrapper(unary, nextString, replCtx, state)
					if err != nil {
						return nil, obj.Pos, err
					}
					return Not(boolEvaluator, replCtx, state), obj.Pos, nil
				case "==":
					boolEvaluator, err = StringEqualsWrapper(unary, nextString, replCtx, state)
					if err != nil {
						return nil, obj.Pos, err
					}
					return boolEvaluator, obj.Pos, nil
				case "=~":
					if nextString.EvalFnc != nil {
						return nil, obj.Pos, &ErrNonStaticPattern{Field: nextString.Field}
					}

					// force pattern if needed
					if nextString.ValueType == ScalarValueType {
						nextString.ValueType = PatternValueType
					}

					boolEvaluator, err = StringEqualsWrapper(unary, nextString, replCtx, state)
					if err != nil {
						return nil, obj.Pos, err
					}
					return boolEvaluator, obj.Pos, nil
				}
				return nil, pos, NewOpUnknownError(obj.Pos, *obj.ScalarComparison.Op)
			case *CIDREvaluator:
				switch next.(type) {
				case *CIDREvaluator:
					nextIP, ok := next.(*CIDREvaluator)
					if !ok {
						return nil, pos, NewTypeError(pos, reflect.TypeOf(CIDREvaluator{}).Kind())
					}

					switch *obj.ScalarComparison.Op {
					case "!=":
						boolEvaluator, err = CIDREquals(unary, nextIP, replCtx, state)
						if err != nil {
							return nil, obj.Pos, err
						}
						return Not(boolEvaluator, replCtx, state), obj.Pos, nil
					case "==":
						boolEvaluator, err = CIDREquals(unary, nextIP, replCtx, state)
						if err != nil {
							return nil, obj.Pos, err
						}
						return boolEvaluator, obj.Pos, nil
					}
					return nil, pos, NewOpUnknownError(obj.Pos, *obj.ScalarComparison.Op)
				}
			case *StringArrayEvaluator:
				nextString, ok := next.(*StringEvaluator)
				if !ok {
					return nil, pos, NewTypeError(pos, reflect.String)
				}

				switch *obj.ScalarComparison.Op {
				case "!=":
					boolEvaluator, err = StringArrayContainsWrapper(nextString, unary, replCtx, state)
					if err != nil {
						return nil, obj.Pos, err
					}
					return Not(boolEvaluator, replCtx, state), obj.Pos, nil
				case "==":
					boolEvaluator, err = StringArrayContainsWrapper(nextString, unary, replCtx, state)
					if err != nil {
						return nil, obj.Pos, err
					}
					return boolEvaluator, obj.Pos, nil
				case "!~":
					if nextString.EvalFnc != nil {
						return nil, obj.Pos, &ErrNonStaticPattern{Field: nextString.Field}
					}

					// force pattern if needed
					if nextString.ValueType == ScalarValueType {
						nextString.ValueType = PatternValueType
					}

					boolEvaluator, err = StringArrayContainsWrapper(nextString, unary, replCtx, state)
					if err != nil {
						return nil, obj.Pos, err
					}
					return Not(boolEvaluator, replCtx, state), obj.Pos, nil
				case "=~":
					if nextString.EvalFnc != nil {
						return nil, obj.Pos, &ErrNonStaticPattern{Field: nextString.Field}
					}

					// force pattern if needed
					if nextString.ValueType == ScalarValueType {
						nextString.ValueType = PatternValueType
					}

					boolEvaluator, err = StringArrayContainsWrapper(nextString, unary, replCtx, state)
					if err != nil {
						return nil, obj.Pos, err
					}
					return boolEvaluator, obj.Pos, nil
				}
			case *IntEvaluator:
				switch nextInt := next.(type) {
				case *IntEvaluator:
					if nextInt.isDuration {
						switch *obj.ScalarComparison.Op {
						case "<":
							boolEvaluator, err = DurationLesserThan(unary, nextInt, replCtx, state)
							if err != nil {
								return nil, obj.Pos, err
							}
							return boolEvaluator, obj.Pos, nil
						case "<=":
							boolEvaluator, err = DurationLesserOrEqualThan(unary, nextInt, replCtx, state)
							if err != nil {
								return nil, obj.Pos, err
							}
							return boolEvaluator, obj.Pos, nil
						case ">":
							boolEvaluator, err = DurationGreaterThan(unary, nextInt, replCtx, state)
							if err != nil {
								return nil, obj.Pos, err
							}
							return boolEvaluator, obj.Pos, nil
						case ">=":
							boolEvaluator, err = DurationGreaterOrEqualThan(unary, nextInt, replCtx, state)
							if err != nil {
								return nil, obj.Pos, err
							}
							return boolEvaluator, obj.Pos, nil
						}
					} else {
						switch *obj.ScalarComparison.Op {
						case "<":
							boolEvaluator, err = LesserThan(unary, nextInt, replCtx, state)
							if err != nil {
								return nil, obj.Pos, err
							}
							return boolEvaluator, obj.Pos, nil
						case "<=":
							boolEvaluator, err = LesserOrEqualThan(unary, nextInt, replCtx, state)
							if err != nil {
								return nil, obj.Pos, err
							}
							return boolEvaluator, obj.Pos, nil
						case ">":
							boolEvaluator, err = GreaterThan(unary, nextInt, replCtx, state)
							if err != nil {
								return nil, obj.Pos, err
							}
							return boolEvaluator, obj.Pos, nil
						case ">=":
							boolEvaluator, err = GreaterOrEqualThan(unary, nextInt, replCtx, state)
							if err != nil {
								return nil, obj.Pos, err
							}
							return boolEvaluator, obj.Pos, nil
						case "!=":
							boolEvaluator, err = IntEquals(unary, nextInt, replCtx, state)
							if err != nil {
								return nil, obj.Pos, err
							}

							return Not(boolEvaluator, replCtx, state), obj.Pos, nil
						case "==":
							boolEvaluator, err = IntEquals(unary, nextInt, replCtx, state)
							if err != nil {
								return nil, obj.Pos, err
							}
							return boolEvaluator, obj.Pos, nil
						default:
							return nil, pos, NewOpUnknownError(obj.Pos, *obj.ScalarComparison.Op)
						}
					}
				case *IntArrayEvaluator:
					nextIntArray := next.(*IntArrayEvaluator)

					switch *obj.ScalarComparison.Op {
					case "<":
						boolEvaluator, err = IntArrayLesserThan(unary, nextIntArray, replCtx, state)
						if err != nil {
							return nil, obj.Pos, err
						}
						return boolEvaluator, obj.Pos, nil
					case "<=":
						boolEvaluator, err = IntArrayLesserOrEqualThan(unary, nextIntArray, replCtx, state)
						if err != nil {
							return nil, obj.Pos, err
						}
						return boolEvaluator, obj.Pos, nil
					case ">":
						boolEvaluator, err = IntArrayGreaterThan(unary, nextIntArray, replCtx, state)
						if err != nil {
							return nil, obj.Pos, err
						}
						return boolEvaluator, obj.Pos, nil
					case ">=":
						boolEvaluator, err = IntArrayGreaterOrEqualThan(unary, nextIntArray, replCtx, state)
						if err != nil {
							return nil, obj.Pos, err
						}
						return boolEvaluator, obj.Pos, nil
					case "!=":
						boolEvaluator, err = IntArrayEquals(unary, nextIntArray, replCtx, state)
						if err != nil {
							return nil, obj.Pos, err
						}
						return Not(boolEvaluator, replCtx, state), obj.Pos, nil
					case "==":
						boolEvaluator, err = IntArrayEquals(unary, nextIntArray, replCtx, state)
						if err != nil {
							return nil, obj.Pos, err
						}
						return boolEvaluator, obj.Pos, nil
					default:
						return nil, pos, NewOpUnknownError(obj.Pos, *obj.ScalarComparison.Op)
					}
				}
				return nil, pos, NewTypeError(pos, reflect.Int)
			case *IntArrayEvaluator:
				nextInt, ok := next.(*IntEvaluator)
				if !ok {
					return nil, pos, NewTypeError(pos, reflect.Int)
				}

				switch *obj.ScalarComparison.Op {
				case "<":
					boolEvaluator, err = IntArrayGreaterThan(nextInt, unary, replCtx, state)
					if err != nil {
						return nil, obj.Pos, err
					}
					return boolEvaluator, obj.Pos, nil
				case "<=":
					boolEvaluator, err = IntArrayGreaterOrEqualThan(nextInt, unary, replCtx, state)
					if err != nil {
						return nil, obj.Pos, err
					}
					return boolEvaluator, obj.Pos, nil
				case ">":
					boolEvaluator, err = IntArrayLesserThan(nextInt, unary, replCtx, state)
					if err != nil {
						return nil, obj.Pos, err
					}
					return boolEvaluator, obj.Pos, nil
				case ">=":
					boolEvaluator, err = IntArrayLesserOrEqualThan(nextInt, unary, replCtx, state)
					if err != nil {
						return nil, obj.Pos, err
					}
					return boolEvaluator, obj.Pos, nil
				case "!=":
					boolEvaluator, err = IntArrayEquals(nextInt, unary, replCtx, state)
					if err != nil {
						return nil, obj.Pos, err
					}
					return Not(boolEvaluator, replCtx, state), obj.Pos, nil
				case "==":
					boolEvaluator, err = IntArrayEquals(nextInt, unary, replCtx, state)
					if err != nil {
						return nil, obj.Pos, err
					}
					return boolEvaluator, obj.Pos, nil
				}
				return nil, pos, NewOpUnknownError(obj.Pos, *obj.ScalarComparison.Op)
			}
		} else {
			return unary, pos, nil
		}

	case *ast.ArrayComparison:
		return nodeToEvaluator(obj.Array, replCtx, state)

	case *ast.ScalarComparison:
		return nodeToEvaluator(obj.Next, replCtx, state)

	case *ast.Unary:
		if obj.Op != nil {
			unary, pos, err = nodeToEvaluator(obj.Unary, replCtx, state)
			if err != nil {
				return nil, pos, err
			}

			switch *obj.Op {
			case "!", "not":
				unaryBool, ok := unary.(*BoolEvaluator)
				if !ok {
					return nil, pos, NewTypeError(pos, reflect.Bool)
				}

				return Not(unaryBool, replCtx, state), obj.Pos, nil
			case "-":
				unaryInt, ok := unary.(*IntEvaluator)
				if !ok {
					return nil, pos, NewTypeError(pos, reflect.Int)
				}

				return Minus(unaryInt, replCtx, state), pos, nil
			case "^":
				unaryInt, ok := unary.(*IntEvaluator)
				if !ok {
					return nil, pos, NewTypeError(pos, reflect.Int)
				}

				return IntNot(unaryInt, replCtx, state), pos, nil
			}
			return nil, pos, NewOpUnknownError(obj.Pos, *obj.Op)
		}

		return nodeToEvaluator(obj.Primary, replCtx, state)
	case *ast.Primary:
		switch {
		case obj.Ident != nil:
			return identToEvaluator(&ident{Pos: obj.Pos, Ident: obj.Ident}, replCtx, state)
		case obj.Number != nil:
			return &IntEvaluator{
				Value: *obj.Number,
			}, obj.Pos, nil
		case obj.Variable != nil:
			varname, ok := isVariableName(*obj.Variable)
			if !ok {
				return nil, obj.Pos, NewError(obj.Pos, fmt.Sprintf("internal variable error '%s'", varname))
			}

			return evaluatorFromVariable(varname, obj.Pos, replCtx)
		case obj.Duration != nil:
			return &IntEvaluator{
				Value:      *obj.Duration,
				isDuration: true,
			}, obj.Pos, nil
		case obj.String != nil:
			str := *obj.String

			// contains variables
			if len(variableRegex.FindAllIndex([]byte(str), -1)) > 0 {
				return stringEvaluatorFromVariable(str, obj.Pos, replCtx)
			}

			return &StringEvaluator{
				Value:     str,
				ValueType: ScalarValueType,
			}, obj.Pos, nil
		case obj.Pattern != nil:
			evaluator := &StringEvaluator{
				Value:     *obj.Pattern,
				ValueType: PatternValueType,
			}
			return evaluator, obj.Pos, nil
		case obj.Regexp != nil:
			evaluator := &StringEvaluator{
				Value:     *obj.Regexp,
				ValueType: RegexpValueType,
			}
			return evaluator, obj.Pos, nil
		case obj.IP != nil:
			ipnet, err := ParseCIDR(*obj.IP)
			if err != nil {
				return nil, obj.Pos, NewError(obj.Pos, fmt.Sprintf("invalid IP '%s'", *obj.IP))
			}

			evaluator := &CIDREvaluator{
				Value:     *ipnet,
				ValueType: IPNetValueType,
			}
			return evaluator, obj.Pos, nil
		case obj.CIDR != nil:
			ipnet, err := ParseCIDR(*obj.CIDR)
			if err != nil {
				return nil, obj.Pos, NewError(obj.Pos, fmt.Sprintf("invalid CIDR '%s'", *obj.CIDR))
			}

			evaluator := &CIDREvaluator{
				Value:     *ipnet,
				ValueType: IPNetValueType,
			}
			return evaluator, obj.Pos, nil
		case obj.SubExpression != nil:
			return nodeToEvaluator(obj.SubExpression, replCtx, state)
		default:
			return nil, obj.Pos, NewError(obj.Pos, fmt.Sprintf("unknown primary '%s'", reflect.TypeOf(obj)))
		}
	case *ast.Array:
		return arrayToEvaluator(obj, replCtx, state)
	}

	return nil, lexer.Position{}, NewError(lexer.Position{}, fmt.Sprintf("unknown entity '%s'", reflect.TypeOf(obj)))
}
