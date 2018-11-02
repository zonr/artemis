/**
 * Copyright (c) 2018, The Artemis Authors.
 *
 * Permission to use, copy, modify, and/or distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package value

import (
	"fmt"
	"reflect"

	"github.com/botobag/artemis/graphql"
	"github.com/botobag/artemis/graphql/ast"
	"github.com/botobag/artemis/internal/util"
)

type valuePath struct {
	prev *valuePath
	key  interface{}
}

func (path *valuePath) String() string {
	var s string
	for path != nil {
		switch key := path.key.(type) {
		case string:
			s = fmt.Sprintf(".%s%s", key, s)
		case int:
			s = fmt.Sprintf("[%d]%s", key, s)
		}
		path = path.prev
	}
	if len(s) > 0 {
		return "value" + s
	}
	return s
}

func (path *valuePath) NewListIndex(index int) *valuePath {
	return &valuePath{
		prev: path,
		key:  index,
	}
}

func (path *valuePath) NewObjectField(name string) *valuePath {
	return &valuePath{
		prev: path,
		key:  name,
	}
}

func (path *valuePath) Empty() bool {
	return path == nil
}

// CoerceValue coerces a Go value given a GraphQL Type.
//
// Returns either a value which is valid for the provided type or a list of encountered coercion
// errors.
func CoerceValue(value interface{}, t graphql.Type, blameNode ast.Node) (interface{}, graphql.Errors) {
	return coerceValueImpl(value, t, blameNode, nil)
}

func coerceValueImpl(value interface{}, t graphql.Type, blameNode ast.Node, path *valuePath) (interface{}, graphql.Errors) {
	// A value must be provided if the type is non-null.
	if nonNullType, isNonNullType := t.(*graphql.NonNull); isNonNullType {
		if value == nil {
			return nil, graphql.Errors{
				newCoercionError(
					fmt.Sprintf(`Expected non-nullable type %v not to be null`, t),
					blameNode,
					path,
					"",  /* subMessage */
					nil, /* originalError */
				)}
		}
		return coerceValueImpl(value, nonNullType.InnerType(), blameNode, path)
	}

	if value == nil {
		// Explicitly return the value null.
		return nil, nil
	}

	switch t := t.(type) {
	case *graphql.Scalar:
		// Scalars determine if a value is valid via CoerceVariableValue(), which returns error to
		// indicate failure. If it returns the error, maintain a reference to the original error.
		coerced, err := t.CoerceVariableValue(value)
		if err != nil {
			var subMessage string
			if e, ok := err.(*graphql.Error); ok && e.Kind == graphql.ErrKindCoercion {
				// Include the message in the error message.
				subMessage = e.Message
			}
			return nil, graphql.Errors{
				newCoercionError(
					fmt.Sprintf(`Expected type %s`, t.String()),
					blameNode,
					path,
					subMessage,
					err,
				)}
		}
		return coerced, nil

	case *graphql.Enum:
		coerced, err := t.CoerceVariableValue(value)
		if err != nil {
			enumValues := make([]string, len(t.Values()))
			for i, enumValue := range t.Values() {
				enumValues[i] = enumValue.Name()
			}
			suggestions := util.SuggestionList(fmt.Sprintf("%v", value), enumValues)

			var didYouMean string
			if len(suggestions) > 0 {
				didYouMean = fmt.Sprintf("did you mean %s?",
					util.OrList(suggestions, 5 /* maxLength */, false /* quoted */))
			}
			return nil, graphql.Errors{
				newCoercionError(
					fmt.Sprintf(`Expected type %s`, t.String()),
					blameNode,
					path,
					didYouMean,
					err,
				)}
		}
		return coerced, nil

	case *graphql.List:
		elementType := t.ElementType()
		reflectValue := reflect.ValueOf(value)
		if reflectValue.Kind() == reflect.Slice || reflectValue.Kind() == reflect.Array {
			numElements := reflectValue.Len()
			if numElements == 0 {
				return []interface{}{}, nil
			}

			var errs graphql.Errors
			// Allocate storage for coerced values.
			coercedValues := make([]interface{}, 0, numElements)
			// Allocate a path key for adding list index to the path.
			path := path.NewListIndex(0)
			for i := 0; i < numElements; i++ {
				path.key = i
				coercedValue, elementErrs := coerceValueImpl(
					reflectValue.Index(i).Interface(),
					elementType,
					blameNode,
					path)
				if len(elementErrs) > 0 {
					errs = append(errs, elementErrs...)
				} else if len(errs) == 0 {
					coercedValues = append(coercedValues, coercedValue)
				}
			}
			if len(errs) > 0 {
				return nil, errs
			}
			return coercedValues, nil
		}

		// Lists accept a non-list value as a list of one.
		coercedValue, errs := coerceValueImpl(value, elementType, blameNode, path)
		if len(errs) > 0 {
			return nil, errs
		}
		return []interface{}{coercedValue}, nil

	case *graphql.InputObject:
		// Currently we only accept map[string]interface{}. See #52.
		objectValue, isObjectValue := value.(map[string]interface{})
		if !isObjectValue {
			return nil, graphql.Errors{
				newCoercionError(
					fmt.Sprintf(`Expected type %s to be an object`, t.String()),
					blameNode,
					path,
					"", /* subMessage */
					graphql.NewError(fmt.Sprintf("value for InputObject should be given in a map[string]interface{}, but got: %T", value)),
				)}
		}

		var errs graphql.Errors
		fields := t.Fields()
		coercedValue := make(map[string]interface{}, len(fields))
		// Allocate a path key for adding field name to the path.
		path := path.NewObjectField("")

		// Ensure every defined field is valid.
		for name, field := range fields {
			fieldValue, hasFieldValue := objectValue[name]
			path.key = name
			if !hasFieldValue {
				if field.HasDefaultValue() {
					coercedValue[name] = field.DefaultValue()
				} else if graphql.IsNonNullType(field.Type()) {
					errs = append(errs,
						newCoercionError(
							fmt.Sprintf(`Field %s of required type %v was not provided`, path.String(), field.Type()),
							blameNode,
							nil, /* path */
							"",  /* subMessage */
							nil, /* originalError */
						))
				}
			} else {
				coercedField, fieldErrs := coerceValueImpl(fieldValue, field.Type(), blameNode, path)
				if len(fieldErrs) > 0 {
					errs = append(errs, fieldErrs...)
				} else if len(errs) == 0 {
					coercedValue[name] = coercedField
				}
			}
		}

		// Restore path.
		path = path.prev

		// Ensure every provided field is defined.
		var fieldNames []string
		for name := range objectValue {
			_, exists := fields[name]
			if !exists {
				if fieldNames == nil {
					// Collect field names.
					fieldNames = make([]string, 0, len(fields))
					for name := range fields {
						fieldNames = append(fieldNames, name)
					}
				}
				suggestions := util.SuggestionList(name, fieldNames)
				var didYouMean string
				if len(suggestions) > 0 {
					didYouMean = fmt.Sprintf("did you mean %s?", util.OrList(suggestions, 5 /* maxLength*/, false /*quoted*/))
				}

				errs = append(errs,
					newCoercionError(
						fmt.Sprintf(`Field "%s" is not defined by type %s`, name, t.String()),
						blameNode,
						path,
						didYouMean,
						nil, /* originalError */
					))
			}
		}

		if len(errs) > 0 {
			return nil, errs
		}
		return coercedValue, nil
	}

	return nil, graphql.Errors{
		newCoercionError(
			fmt.Sprintf("%v is not a valid input type", t),
			blameNode,
			path,
			"",  /* subMessage */
			nil, /* originalError */
		),
	}
}

func newCoercionError(
	message string,
	blameNode ast.Node,
	path *valuePath,
	subMessage string,
	originalError error) *graphql.Error {
	var messageBuilder util.StringBuilder

	messageBuilder.WriteString(message)
	if !path.Empty() {
		messageBuilder.WriteString(" at ")
		messageBuilder.WriteString(path.String())
	}

	if len(subMessage) > 0 {
		messageBuilder.WriteString("; ")
		messageBuilder.WriteString(subMessage)
	} else {
		messageBuilder.WriteRune('.')
	}

	var locations []graphql.ErrorLocation
	if blameNode != nil {
		locations = []graphql.ErrorLocation{
			graphql.ErrorLocationOfASTNode(blameNode),
		}
	}

	if originalError == nil {
		// XXX
		return graphql.NewError(
			messageBuilder.String(),
			locations).(*graphql.Error)
	}

	return graphql.NewError(
		messageBuilder.String(),
		locations,
		originalError).(*graphql.Error)
}
