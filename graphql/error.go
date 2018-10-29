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

package graphql

import (
	"encoding/json"
	"fmt"
	"log"
	"reflect"
	"runtime"
	"strconv"

	"github.com/botobag/artemis/internal/util"
)

// Op describes an operation, usually as the package and method, such as "language/parser.Parse".
type Op string

// ErrKind defines the kind of error this is.
type ErrKind uint8

// Enumeration of Kind
const (
	ErrKindOther      ErrKind = iota // Unclassified error. This value is not printed in the error message.
	ErrKindCoercion                  // Failed to cerce input or result values for desired GraphQL type.
	ErrKindSyntax                    // Represent a syntax error in the GraphQL source.
	ErrKindValidation                // Represent an error occurred when validating schema.
	ErrKindExecution                 // Represent an error occurred when executing a query.
	ErrKindInternal                  // Internal error
)

func (k ErrKind) String() string {
	switch k {
	case ErrKindOther:
		return "other error"
	case ErrKindCoercion:
		return "coercion error"
	case ErrKindSyntax:
		return "syntax error"
	case ErrKindValidation:
		return "validation error"
	case ErrKindExecution:
		return "execution error"
	case ErrKindInternal:
		return "internal error"
	}
	return "unknown error kind"
}

// ErrorExtensions provides an additional entry to a GraphQL error with key "extensions". It is
// useful for attaching vendor-specific error data (such as error code).
//
// Ref: https://github.com/facebook/graphql/pull/407
type ErrorExtensions map[string]interface{}

// ErrorLocation contains a line number and a column number to point out the beginning of an
// associated syntax element.
type ErrorLocation struct {
	// Both line and column are positive numbers starting from 1
	Line   uint `json:"line"`
	Column uint `json:"column"`
}

// ErrorWithLocations indicates an error that contains locations. If "locations" is not given in the
// arguments to NewError, NewError will retrieve one from the underlying error (if provided) that
// implements this interface.
type ErrorWithLocations interface {
	Locations() []ErrorLocation
}

// ResponsePath is an array of "key" where each key is either a string (indicating the field name)
// or an integer (indicating an index to list.) It should be presented when an error can be assoc
type ResponsePath struct {
	// Currently this could only be either int or string.
	keys []interface{}
}

// AppendFieldName adds a field name to the end of current path.
func (path *ResponsePath) AppendFieldName(name string) {
	path.keys = append(path.keys, name)
}

// AppendIndex adds a list index to the end of current path.
func (path *ResponsePath) AppendIndex(index int) {
	path.keys = append(path.keys, index)
}

// Clone makes a deep copy of the path.
func (path *ResponsePath) Clone() *ResponsePath {
	if len(path.keys) == 0 {
		return &ResponsePath{}
	}

	keys := make([]interface{}, len(path.keys))
	copy(keys, path.keys)
	return &ResponsePath{keys}
}

// MarshalJSON serializes path keys to JSON.
func (path *ResponsePath) MarshalJSON() ([]byte, error) {
	return json.Marshal(path.keys)
}

// String serializes a ResponsePath to more readable format.
func (path *ResponsePath) String() string {
	var b util.StringBuilder
	for _, key := range path.keys {
		switch key := key.(type) {
		case string:
			// Field name
			if b.Len() > 0 {
				b.WriteRune('.')
			}
			b.WriteString(key)

		case int:
			// Index
			b.WriteRune('[')
			b.WriteString(strconv.FormatInt(int64(key), 10))
			b.WriteRune(']')

			// Other types should never happen.
		}
	}
	return b.String()
}

// ErrorWithPath indicates an error that contains a path for reporting. If "path" is not given in
// the arguments to NewError, NewError will retrieve the one from the underlying error (if provided)
// that implements this interface.
type ErrorWithPath interface {
	Path() *ResponsePath
}

// ErrorWithExtensions indicates an error that contains extensions data. If "extensions" is not
// given in the arguments to NewError, NewError will retrieve the one from the underlying error (if
// provided) that implements this interface.
type ErrorWithExtensions interface {
	Extensions() ErrorExtensions
}

// An Error describes an error found during parse, validate or execute phases of performing a
// GraphQL operation. It can be serialized to JSON for including in the response.
//
// The Error is designed to contain the fields defined in the specification [0]. Furthermore, you
// can build an Error by wrapping an error value. Information (if unspecified in the arguments to
// NewError) in the error value will be propagated to the newly created Error. During the execution,
// the Error value is returned alone the execution path to the top, each intermediate function will
// either pass through the error to its caller or could wrap the error with further information, or
// even rewrite the error.
//
// It also includes Op and ErrKind which will show when printing the error value. This makes it
// helpful for programmers.
//
// [0] https://facebook.github.io/graphql/June2018/#sec-Errors
type Error struct {
	// Message describes the error for debugging purposes. It is required by a GraphQL Error as per
	// spec..
	Message string `json:"message"`

	// Locations is an array of { line, column } locations within the source GraphQL document which
	// correspond to this error. It should be included if an error can be associated to a particular
	// point in the requested GraphQL document as per spec..
	//
	// Errors during validation often contain multiple locations, for example to point out two things
	// with the same name. Errors during execution include a single location, the field which produced
	// the error.
	Locations []ErrorLocation `json:"locations,omitempty"`

	// Path describes the path of the response field which experienced the error. It should be
	// presented when an error can be associated to a particular field in the GraphQL result as per
	// spec.. Currently, it is only included for errors during execution. See example in [0].
	//
	// [0]: https://facebook.github.io/graphql/June2018/#example-90475
	Path *ResponsePath `json:"path,omitempty"`

	// Extensions contains data to be added to in the error response
	Extensions ErrorExtensions `json:"extensions,omitempty"`

	// The underlying error that triggered this one
	Err error `json:"-"`

	// Op is the operation being performed, usually the name of the method being invoked.
	Op Op `json:"-"`

	// Kind is the class of error
	Kind ErrKind `json:"-"`
}

// Error implements Go error interface.
var _ error = (*Error)(nil)

// NewError builds an error value from arguments. Inspired by the design of upspin.io/errors [0].
//
// [0]: https://commandcenter.blogspot.com/2017/12/error-handling-in-upspin.html.
func NewError(message string, args ...interface{}) error {
	e := &Error{
		Message: message,
	}

	for _, arg := range args {
		switch arg := arg.(type) {
		case ErrorLocation:
			e.Locations = []ErrorLocation{arg}
		case []ErrorLocation:
			e.Locations = arg

		case *ResponsePath:
			e.Path = arg

		case ErrorExtensions:
			e.Extensions = arg

		case error:
			e.Err = arg

		case Op:
			e.Op = arg

		case ErrKind:
			e.Kind = arg

		default:
			_, file, line, _ := runtime.Caller(1)
			log.Printf("NewError: bad call from %s:%d: %v", file, line, args)
			return fmt.Errorf("unknown type %T, value %v in error call", arg, arg)
		}
	}

	// Propagate locations, path or extensions from underlying error when one is not provided in
	// argument.
	prev := e.Err
	if prev != nil {
		if len(e.Locations) == 0 {
			switch errWithLocations := prev.(type) {
			case ErrorWithLocations:
				e.Locations = errWithLocations.Locations()
			case *Error:
				if len(errWithLocations.Locations) > 0 {
					e.Locations = make([]ErrorLocation, len(errWithLocations.Locations))
					copy(e.Locations, errWithLocations.Locations)
				}
			}
		}

		if e.Path == nil {
			switch errWithPath := prev.(type) {
			case ErrorWithPath:
				e.Path = errWithPath.Path()
			case *Error:
				if errWithPath.Path != nil {
					e.Path = errWithPath.Path.Clone()
				}
			}
		}

		if e.Extensions == nil {
			switch errWithExtensions := prev.(type) {
			case ErrorWithExtensions:
				e.Extensions = errWithExtensions.Extensions()
			case *Error:
				e.Extensions = errWithExtensions.Extensions
			}
		}

		// Pull kind from underlying error.
		if e.Kind == ErrKindOther {
			if prev, ok := prev.(*Error); ok {
				e.Kind = prev.Kind
			}
		}
	}

	return e
}

// WrapError is a convenient wrapper to build an Error value from an underlying error with a
// message.
func WrapError(err error, message string) error {
	return NewError(message, err)
}

// WrapErrorf is similar to WrapError but with the format specifier.
func WrapErrorf(err error, format string, args ...interface{}) error {
	return NewError(fmt.Sprintf(format, args...), err)
}

// Error implements Go's error interface.
func (e *Error) Error() string {
	var b util.StringBuilder
	e.printError(&b, nil)
	return b.String()
}

func (e *Error) printError(b *util.StringBuilder, nextErr *Error) {
	// If the previous error was also one of ours. Suppress duplications so the message won't contain
	// the same kind, file name or user name twice.
	initialLen := b.Len()

	// pad appends str to the buffer if the buffer already has some data.
	pad := func(str string) {
		if b.Len() == initialLen {
			return
		}
		b.WriteString(str)
	}

	if len(e.Op) > 0 {
		b.WriteString(string(e.Op))
	}

	if len(e.Message) > 0 {
		pad(": ")
		b.WriteString(e.Message)
	}

	if e.Locations != nil {
		// Don't print location if the next error already did.
		if nextErr == nil || !reflect.DeepEqual(nextErr.Locations, e.Locations) {
			if b.Len() == initialLen {
				b.WriteString("At ")
			} else {
				b.WriteString(" at ")
			}
			b.WriteString(fmt.Sprintf("%+v", e.Locations))
		}
	}

	if e.Path != nil {
		// Don't print path if the next error already did.
		if nextErr == nil || !reflect.DeepEqual(nextErr.Path, e.Path) {
			if b.Len() == initialLen {
				b.WriteString("For ")
			} else {
				b.WriteString(" for ")
			}
			b.WriteString("response field in the path ")
			b.WriteString(e.Path.String())
		}
	}

	if e.Kind != ErrKindOther {
		// Don't print path if the next error has the same kind as ours.
		if nextErr == nil || nextErr.Kind != e.Kind {
			pad(": ")
			b.WriteString(e.Kind.String())
		}
	}

	if len(e.Extensions) > 0 {
		// Don't print extensions if the next error already did.
		if nextErr == nil || !reflect.DeepEqual(nextErr.Extensions, e.Extensions) {
			pad(" (additonal info: ")
			b.WriteString(fmt.Sprintf("%v)", e.Extensions))
		}
	}

	if e.Err != nil {
		if prev, ok := e.Err.(*Error); ok {
			// Indent on new line if we are cascading non-empty Error.
			pad(":\n  ")
			prev.printError(b, e)
		} else {
			pad(": ")
			b.WriteString(e.Err.Error())
		}
	}

	return
}