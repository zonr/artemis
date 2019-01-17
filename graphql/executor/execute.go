/**
 * Copyright (c) 2019, The Artemis Authors.
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

package executor

import (
	"fmt"
	"reflect"

	"github.com/botobag/artemis/graphql"
	"github.com/botobag/artemis/graphql/ast"
	values "github.com/botobag/artemis/graphql/internal/value"
)

// ExecutionResult contains result from running an Executor.
type ExecutionResult struct {
	Data   *ResultNode
	Errors graphql.Errors
}

// Given a selectionSet, adds all of the fields in that selection to the passed in map of fields,
// and returns it at the end.
//
// CollectFields requires the "runtime type" of an object. For a field which returns an Interface or
// Union type, the "runtime type" will be the actual Object type returned by that field.
func collectFields(
	ctx *ExecutionContext,
	node *ExecutionNode,
	runtimeType graphql.Object) ([]*ExecutionNode, error) {
	// Look up nodes for the Selection Set with the given runtime type in node's child nodes.
	var childNodes []*ExecutionNode

	if node.Children == nil {
		// Initialize the children node map.
		node.Children = map[graphql.Object][]*ExecutionNode{}
	} else {
		// See whether we have built one before.
		childNodes = node.Children[runtimeType]
	}

	if childNodes == nil {
		// Load selection set into ExecutionNode's.
		var err error
		childNodes, err = buildChildExecutionNodesForSelectionSet(ctx, node, runtimeType)
		if err != nil {
			return nil, err
		}
	}

	// Store the result before return.
	node.Children[runtimeType] = childNodes

	return childNodes, nil
}

// Build ExecutionNode's for the selection set of given node.
func buildChildExecutionNodesForSelectionSet(
	ctx *ExecutionContext,
	parentNode *ExecutionNode,
	runtimeType graphql.Object) ([]*ExecutionNode, error) {
	// Boolean set to prevent named fragment to be applied twice or more in a selection set.
	visitedFragmentNames := map[string]bool{}

	// Map field response key to its corresponding node; This is used to group field definitions when
	// two fields corresponding to the same response key was requested in the selection set.
	fields := map[string]*ExecutionNode{}

	// The result nodes
	childNodes := []*ExecutionNode{}

	type taskData struct {
		// The Selection Set that is being processed into childNodes
		selectionSet ast.SelectionSet

		// The index of Selection to be resumed when restarting the task.
		selectionIndex int
	}

	// Stack contains task to be processed.
	var stack []taskData

	// Initialize the stack. Find the selection sets in parentNode to process.
	if parentNode.IsRoot() {
		stack = []taskData{
			{ctx.Operation().Definition().SelectionSet, 0},
		}
	} else {
		definitions := parentNode.Definitions
		numDefinitions := len(definitions)
		stack = make([]taskData, numDefinitions)
		// stack is LIFO so place the selection sets in reverse order.
		for i, definition := range definitions {
			stack[numDefinitions-i-1].selectionSet = definition.SelectionSet
		}
	}

	for len(stack) > 0 {
		var (
			data = &stack[len(stack)-1]

			selectionSet  = data.selectionSet
			numSelections = len(selectionSet)
			interrupted   = false
		)

		for data.selectionIndex < numSelections && !interrupted {
			selection := selectionSet[data.selectionIndex]
			data.selectionIndex++
			if data.selectionIndex >= numSelections {
				// No more selections in the selection set. Pop it from the stack.
				stack = stack[:len(stack)-1]
			}

			// Check @skip and @include.
			shouldInclude, err := shouldIncludeNode(ctx, selection)
			if err != nil {
				return nil, err
			} else if !shouldInclude {
				continue
			}

			switch selection := selection.(type) {
			case *ast.Field:
				// Find existing fields.
				name := selection.ResponseKey()
				field := fields[name]
				if field != nil {
					// The field with the same name has been added to the selection set before. Append the
					// definition to the same node to coalesce their selection sets.
					field.Definitions = append(field.Definitions, selection)
				} else {
					// Find corresponding runtime Field definition in current schema.
					fieldDef := findFieldDef(
						ctx.Operation().Schema(),
						runtimeType,
						selection.Name.Value())
					if fieldDef == nil {
						// Schema doesn't contains the field. Note that we should skip the field without an
						// error as per specification.
						//
						// Reference: 3.c. in https://facebook.github.io/graphql/June2018/#ExecuteSelectionSet().
						break
					}

					// Get argument values.
					arguments, err := values.ArgumentValues(fieldDef, selection, ctx.VariableValues())
					if err != nil {
						return nil, err
					}

					// Build a node.
					field = &ExecutionNode{
						Parent:         parentNode,
						Definitions:    []*ast.Field{selection},
						Field:          fieldDef,
						ArgumentValues: arguments,
					}

					// Add to result.
					childNodes = append(childNodes, field)

					// Insert a map entry.
					fields[name] = field
				}

			case *ast.InlineFragment:
				// Apply fragment only if the runtime type satisfied the type condition.
				if selection.HasTypeCondition() {
					if !doesTypeConditionSatisfy(ctx, selection.TypeCondition, runtimeType) {
						break
					}
				}

				// Push a task to process selection set in the fragment.
				stack = append(stack, taskData{
					selectionSet: selection.SelectionSet,
				})

				// Interrupt current loop to start processing the selection set in the fragment.
				// Specification requires fields to be sorted in DFS order.
				interrupted = true

			case *ast.FragmentSpread:
				fragmentName := selection.Name.Value()
				if visited := visitedFragmentNames[fragmentName]; visited {
					break
				}
				visitedFragmentNames[fragmentName] = true

				// Find fragment definition to get type condition and selection set.
				fragmentDef := ctx.Operation().FragmentDef(fragmentName)
				if fragmentDef == nil {
					break
				}

				if !doesTypeConditionSatisfy(ctx, fragmentDef.TypeCondition, runtimeType) {
					break
				}

				// Push a task to process selection set in the fragment.
				stack = append(stack, taskData{
					selectionSet: fragmentDef.SelectionSet,
				})

				interrupted = true
			}
		}
	} // for len(stack) > 0 {

	return childNodes, nil
}

// Determines if a field should be included based on the @include and @skip directives, where @skip
// has higher precedence than @include.
//
// Reference: https://facebook.github.io/graphql/June2018/#sec--include
func shouldIncludeNode(ctx *ExecutionContext, node ast.Selection) (bool, error) {
	// Neither @skip nor @include has precedence over the other. In the case that both the @skip and
	// @include directives are provided in on the same the field or fragment, it must be queried only
	// if the @skip condition is false and the @include condition is true. Stated conversely, the
	// field or fragment must not be queried if either the @skip condition is true or the @include
	// condition is false.
	skip, err := values.DirectiveValues(
		graphql.SkipDirective(), node.GetDirectives(), ctx.VariableValues())
	if err != nil {
		return false, err
	}
	shouldSkip := skip.Get("if")
	if shouldSkip != nil && shouldSkip.(bool) {
		return false, nil
	}

	include, err := values.DirectiveValues(
		graphql.IncludeDirective(), node.GetDirectives(), ctx.VariableValues())
	if err != nil {
		return false, err
	}
	shouldInclude := include.Get("if")
	if shouldInclude != nil && !shouldInclude.(bool) {
		return false, nil
	}

	return true, nil
}

// This method looks up the field on the given type definition. It has special casing for the two
// introspection fields, __schema and __typename. __typename is special because it can always be
// queried as a field, even in situations where no other fields are allowed, like on a Union.
// __schema could get automatically added to the query type, but that would require mutating type
// definitions, which would cause issues.
func findFieldDef(
	schema *graphql.Schema,
	parentType graphql.Object,
	fieldName string) graphql.Field {
	// TODO: Deal with special introspection fields.
	return parentType.Fields()[fieldName]
}

// Determines if a type condition is satisfied with the given type.
func doesTypeConditionSatisfy(
	ctx *ExecutionContext,
	typeCondition ast.NamedType,
	t graphql.Type) bool {
	schema := ctx.Operation().Schema()

	conditionalType := schema.TypeFromAST(typeCondition)
	if conditionalType == t {
		return true
	}

	if abstractType, ok := t.(graphql.AbstractType); ok {
		for _, possibleType := range schema.PossibleTypes(abstractType) {
			if possibleType == t {
				return true
			}
		}
	}

	return false
}

func collectAndDispatchRootTasks(ctx *ExecutionContext, executor executor) (*ResultNode, error) {
	rootType := ctx.Operation().RootType()
	// Root node is a special node which behaves like a field with nil parent and definition.
	rootNode := &ExecutionNode{
		Parent:      nil,
		Definitions: nil,
	}

	// Collect fields in the top-level selection set.
	nodes, err := collectFields(ctx, rootNode, rootType)
	if err != nil {
		return nil, err
	}

	// Allocate result node.
	result := &ResultNode{}

	// Create tasks for executing root nodes.
	dispatchTasksForObject(
		ctx,
		executor,
		result,
		nodes,
		ctx.RootValue())

	return result, nil
}

// Dispatch tasks for evaluating an object value comprised of the fields specified in childNodes.
// Return the ResultNode that contains
func dispatchTasksForObject(
	ctx *ExecutionContext,
	executor executor,
	result *ResultNode,
	childNodes []*ExecutionNode,
	value interface{}) {

	numChildNodes := len(childNodes)

	// Allocate ResultNode's for each nodes.
	nodeResults := make([]ResultNode, numChildNodes)

	// Setup result value.
	result.Kind = ResultKindObject
	result.Value = &ObjectResultValue{
		ExecutionNodes: childNodes,
		FieldValues:    nodeResults,
	}

	// Create tasks to resolve object fields.
	for i := 0; i < numChildNodes; i++ {
		nodeResult := &nodeResults[i]
		nodeResult.Parent = result
		childNode := childNodes[i]

		// Set the flag so field can reject nil value on error.
		if graphql.IsNonNullType(childNode.Field.Type()) {
			nodeResult.SetIsNonNull()
		}

		// Create a task and dispatch it with given dispatcher.
		executor.Dispatch(&ExecuteNodeTask{
			executor: executor,
			ctx:      ctx,
			node:     childNode,
			result:   nodeResult,
			source:   value,
		})
	}
}

//===----------------------------------------------------------------------------------------====//
// ExecuteNodeTask
//===----------------------------------------------------------------------------------------====//

// ExecuteNodeTask manages
type ExecuteNodeTask struct {
	// Executor that runs this task
	executor executor

	// Context for execution
	ctx *ExecutionContext

	// The node to evaluate
	node *ExecutionNode

	// The ResultNode for writting the field value. It is allocated by the one that prepares the
	// ExecuteNodeTask for execution.
	result *ResultNode

	// Source value which is passed to the field resolver; This is the field value of the parent.
	source interface{}
}

// Run executes the task to value for the field corresponding to the ExecutionNode. The execution
// result is written to the task.result and errors are added to ctx with ctx.AppendErrors so nothing
// is returned from this method.
func (task *ExecuteNodeTask) Run() {
	var (
		ctx    = task.ctx
		node   = task.node
		result = task.result
		field  = node.Field
	)

	// Get field resolver to execute.
	resolver := field.Resolver()
	if resolver == nil {
		resolver = ctx.Operation().DefaultFieldResolver()
	}

	// Create ResolveInfo.
	info := &ResolveInfo{
		ExecutionContext: ctx,
		ExecutionNode:    node,
		ResultNode:       result,
	}

	// Execute resolver to retrieve the field value
	value, err := resolver.Resolve(ctx.ctx, task.source, info)
	if err != nil {
		task.handleNodeError(err, result)
		return
	}

	// Complete subfields with value.
	task.completeValue(field.Type(), task.result, value)

	return
}

// handleNodeError first creates a graphql.Error for an error value (which includes additional
// information such as field location) to be included in the GraphQL response and then adds the
// error to the ctx (using ctx.AppendErrors) to indicate a failed field execution.
func (task *ExecuteNodeTask) handleNodeError(err error, result *ResultNode) {
	node := task.node

	// Attach location info.
	locations := make([]graphql.ErrorLocation, len(node.Definitions))
	for i := range node.Definitions {
		locations[i] = graphql.ErrorLocationOfASTNode(node.Definitions[i])
	}

	// Compute response path.
	path := result.Path()

	// Wrap it as a graphql.Error to ensure a consistent Error interface.
	e, ok := err.(*graphql.Error)
	if !ok {
		e = graphql.NewError(err.Error(), locations, path).(*graphql.Error)
	} else {
		e.Locations = locations
		e.Path = path
	}

	// Set result value to a nil value.
	result.Kind = ResultKindNil
	result.Value = nil

	// Append error to task.errs.
	task.executor.AppendError(e, result)
}

// completeValue implements "Value Completion" [0]. It ensures the value resolved from the field
// resolver adheres to the expected return type.
//
// [0]: https://facebook.github.io/graphql/June2018/#sec-Value-Completion
func (task *ExecuteNodeTask) completeValue(
	returnType graphql.Type,
	result *ResultNode,
	value interface{}) {

	if wrappingType, isWrappingType := returnType.(graphql.WrappingType); isWrappingType {
		task.completeWrappingValue(wrappingType, result, value)
	} else {
		task.completeNonWrappingValue(returnType, result, value)
	}
}

// completeWrappingValue completes value for NonNull and List type.
func (task *ExecuteNodeTask) completeWrappingValue(
	returnType graphql.WrappingType,
	result *ResultNode,
	value interface{}) {

	// Resolvers can return error to signify failure. See https://github.com/graphql/graphql-js/commit/f62c0a25.
	if err, ok := value.(*graphql.Error); ok && err != nil {
		task.handleNodeError(err, result)
		return
	}

	type ValueNode struct {
		returnType graphql.WrappingType
		result     *ResultNode
		value      interface{}
	}
	queue := []ValueNode{
		{
			returnType: returnType,
			result:     result,
			value:      value,
		},
	}

	for len(queue) > 0 {
		var valueNode *ValueNode
		// Pop one value node from queue.
		valueNode, queue = &queue[0], queue[1:]

		var (
			returnType graphql.Type = valueNode.returnType
			result                  = valueNode.result
			value                   = valueNode.value
		)

		// If the parent was resolved to nil, stop processing this node.
		if result.Parent.IsNil() {
			continue
		}

		// Handle non-null.
		nonNullType, isNonNullType := returnType.(graphql.NonNull)

		if isNonNullType {
			// For non-null type, continue on its unwrapped type.
			returnType = nonNullType.InnerType()
		}

		// Handle nil value.
		if values.IsNullish(value) {
			// Check for non-nullability.
			if isNonNullType {
				node := task.node
				task.handleNodeError(
					graphql.NewError(fmt.Sprintf("Cannot return null for non-nullable field %v.%s.",
						parentFieldType(task.ctx, node).Name(), node.Field.Name())),
					result)
			} else {
				// Resolve the value to nil without error.
				result.Kind = ResultKindNil
				result.Value = nil
			}

			// Continue to the next value.
			continue
		} // if values.IsNullish(value)

		listType, isListType := returnType.(graphql.List)
		if !isListType {
			task.completeNonWrappingValue(returnType, result, value)
			continue
		}

		// Complete a list value by completing each item in the list with the inner type.
		v := reflect.ValueOf(value)
		if v.Kind() == reflect.Ptr {
			v = v.Elem()
		}

		if v.Kind() != reflect.Array && v.Kind() != reflect.Slice {
			node := task.node
			task.handleNodeError(
				graphql.NewError(
					fmt.Sprintf("Expected Iterable, but did not find one for field %s.%s.",
						parentFieldType(task.ctx, node).Name(), node.Field.Name())),
				result)
			continue
		}

		elementType := listType.ElementType()
		elementWrappingType, isWrappingElementType := elementType.(graphql.WrappingType)

		// Setup result nodes for elements.
		numElements := v.Len()
		resultNodes := make([]ResultNode, numElements)

		// Set child results to reject nil value if it is unwrapped from a non-null type.
		if isNonNullType {
			for i := range resultNodes {
				resultNodes[i].SetIsNonNull()
			}
		}

		// Complete result.
		result.Kind = ResultKindList
		result.Value = resultNodes

		if isWrappingElementType {
			for i := range resultNodes {
				resultNode := &resultNodes[i]
				resultNode.Parent = result
				queue = append(queue, ValueNode{
					returnType: elementWrappingType,
					result:     resultNode,
					value:      v.Index(i).Interface(),
				})
			}
		} else {
			for i := range resultNodes {
				resultNode := &resultNodes[i]
				resultNode.Parent = result
				value := v.Index(i).Interface()
				if !task.completeNonWrappingValue(elementType, resultNode, value) {
					// If the err causes the parent to be nil'ed, stop procsessing the remaining elements.
					if result.IsNil() {
						break
					}
				}
			}
		}
	}
}

func (task *ExecuteNodeTask) completeNonWrappingValue(
	returnType graphql.Type,
	result *ResultNode,
	value interface{}) (ok bool) {

	// Chack for nullish. Non-null type should already be handled by completeWrappingValue.
	if values.IsNullish(value) {
		result.Value = nil
		result.Kind = ResultKindNil
		return true
	}

	// Resolvers can return error to signify failure. See https://github.com/graphql/graphql-js/commit/f62c0a25.
	if err, ok := value.(*graphql.Error); ok {
		task.handleNodeError(err, result)
		return false
	}

	switch returnType := returnType.(type) {
	// Scalar and Enum
	case graphql.LeafType:
		return task.completeLeafValue(returnType, result, value)

	case graphql.Object:
		return task.completeObjectValue(returnType, result, value)

	// Union and Interface
	case graphql.AbstractType:
		return task.completeAbstractValue(returnType, result, value)
	}

	task.handleNodeError(
		graphql.NewError(fmt.Sprintf(`Cannot complete value of unexpected type "%v".`, returnType)),
		result)
	return false
}

func (task *ExecuteNodeTask) completeLeafValue(
	returnType graphql.LeafType,
	result *ResultNode,
	value interface{}) (ok bool) {

	coercedValue, err := returnType.CoerceResultValue(value)
	if err != nil {
		// See comments in graphql.NewCoercionError for the rules of handling error.
		if e, ok := err.(*graphql.Error); !ok || e.Kind != graphql.ErrKindCoercion {
			// Wrap the error in our own.
			err = graphql.NewDefaultResultCoercionError(returnType.Name(), value, err)
		}
		task.handleNodeError(err, result)
		return false
	}

	// Setup result and return.
	result.Kind = ResultKindLeaf
	result.Value = coercedValue
	return true
}

func (task *ExecuteNodeTask) completeObjectValue(
	returnType graphql.Object,
	result *ResultNode,
	value interface{}) (ok bool) {

	ctx := task.ctx

	// Collect fields in the selection set.
	childNodes, err := collectFields(ctx, task.node, returnType)
	if err != nil {
		task.handleNodeError(err, result)
		return false
	}

	// Dispatch tasks to execute subfields.
	dispatchTasksForObject(task.ctx, task.executor, result, childNodes, value)

	return true
}

func (task *ExecuteNodeTask) completeAbstractValue(
	returnType graphql.AbstractType,
	result *ResultNode,
	value interface{}) (ok bool) {
	panic("unimplemented")
}