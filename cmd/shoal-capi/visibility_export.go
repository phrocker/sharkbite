package main

/*
#cgo CFLAGS: -I${SRCDIR}/../../capi/include
#include "bridge.h"
#include <string.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/phrocker/shoal-oss/accumulo"
)

var (
	columnVisibilityValues = newRFileRegistry[accumulo.ColumnVisibility]()
	visibilityNodeValues   = newRFileRegistry[accumulo.VisibilityNode]()
	nodeExpressionValues   = newRFileRegistry[accumulo.NodeExpression]()
	visibilityEvaluators   = newRFileRegistry[accumulo.VisibilityEvaluator]()
)

//export shoal_visibility_node_view_init
func shoal_visibility_node_view_init(view *C.shoal_visibility_node_view) {
	if view != nil {
		C.memset(unsafe.Pointer(view), 0, C.size_t(C.SHOAL_VISIBILITY_NODE_VIEW_V1_SIZE))
		view.struct_size = C.SHOAL_VISIBILITY_NODE_VIEW_V1_SIZE
	}
}

//export shoal_visibility_parse_error_view_init
func shoal_visibility_parse_error_view_init(view *C.shoal_visibility_parse_error_view) {
	if view != nil {
		C.memset(unsafe.Pointer(view), 0, C.size_t(C.SHOAL_VISIBILITY_PARSE_ERROR_VIEW_V1_SIZE))
		view.struct_size = C.SHOAL_VISIBILITY_PARSE_ERROR_VIEW_V1_SIZE
	}
}

func visibilityBytes(input C.shoal_bytes, name string) ([]byte, error) {
	return copyByteValue(input, name)
}

func allocVisibilityBytes(value []byte, out **C.shoal_bytes_result, outError **C.shoal_error) C.shoal_status {
	if out != nil {
		*out = nil
	}
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	result, code, err := allocBytes(string(value))
	if err != nil {
		return fail(outError, code, err)
	}
	*out = result
	return C.SHOAL_STATUS_OK
}

func addColumnVisibility(value *accumulo.ColumnVisibility, out **C.shoal_column_visibility, outError **C.shoal_error) C.shoal_status {
	id, ok := columnVisibilityValues.add(value)
	if !ok {
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: column visibility handle space exhausted"))
	}
	handle := C.shoal_bridge_column_visibility_alloc(C.uint64_t(id))
	if handle == nil {
		columnVisibilityValues.remove(id)
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate column visibility handle"))
	}
	*out = handle
	return C.SHOAL_STATUS_OK
}

func addVisibilityNode(value accumulo.VisibilityNode, out **C.shoal_visibility_node, outError **C.shoal_error) C.shoal_status {
	id, ok := visibilityNodeValues.add(&value)
	if !ok {
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: visibility node handle space exhausted"))
	}
	handle := C.shoal_bridge_visibility_node_alloc(C.uint64_t(id))
	if handle == nil {
		visibilityNodeValues.remove(id)
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate visibility node handle"))
	}
	*out = handle
	return C.SHOAL_STATUS_OK
}

func addNodeExpression(value accumulo.NodeExpression, out **C.shoal_node_expression, outError **C.shoal_error) C.shoal_status {
	id, ok := nodeExpressionValues.add(&value)
	if !ok {
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: node expression handle space exhausted"))
	}
	handle := C.shoal_bridge_node_expression_alloc(C.uint64_t(id))
	if handle == nil {
		nodeExpressionValues.remove(id)
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate node expression handle"))
	}
	*out = handle
	return C.SHOAL_STATUS_OK
}

func lookupColumnVisibility(handle *C.shoal_column_visibility) (*accumulo.ColumnVisibility, error) {
	if handle == nil {
		return nil, errors.New("shoal: column visibility handle is NULL")
	}
	id := uint64(C.shoal_bridge_column_visibility_id(handle))
	value, ok := columnVisibilityValues.get(id)
	if id == 0 || !ok {
		return nil, errors.New("shoal: column visibility handle is unknown or freed")
	}
	return value, nil
}

func lookupVisibilityNode(handle *C.shoal_visibility_node) (*accumulo.VisibilityNode, error) {
	if handle == nil {
		return nil, errors.New("shoal: visibility node handle is NULL")
	}
	id := uint64(C.shoal_bridge_visibility_node_id(handle))
	value, ok := visibilityNodeValues.get(id)
	if id == 0 || !ok {
		return nil, errors.New("shoal: visibility node handle is unknown or freed")
	}
	return value, nil
}

func lookupNodeExpression(handle *C.shoal_node_expression) (*accumulo.NodeExpression, error) {
	if handle == nil {
		return nil, errors.New("shoal: node expression handle is NULL")
	}
	id := uint64(C.shoal_bridge_node_expression_id(handle))
	value, ok := nodeExpressionValues.get(id)
	if id == 0 || !ok {
		return nil, errors.New("shoal: node expression handle is unknown or freed")
	}
	return value, nil
}

func lookupVisibilityEvaluator(handle *C.shoal_visibility_evaluator) (*accumulo.VisibilityEvaluator, error) {
	if handle == nil {
		return nil, errors.New("shoal: visibility evaluator handle is NULL")
	}
	id := uint64(C.shoal_bridge_visibility_evaluator_id(handle))
	value, ok := visibilityEvaluators.get(id)
	if id == 0 || !ok {
		return nil, errors.New("shoal: visibility evaluator handle is unknown or freed")
	}
	return value, nil
}

//export shoal_column_visibility_create
func shoal_column_visibility_create(expression C.shoal_bytes, out **C.shoal_column_visibility, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_visibility is required"))
	}
	value, err := visibilityBytes(expression, "visibility expression")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	visibility, err := accumulo.NewColumnVisibility(value)
	if err != nil {
		return failForError(outError, err)
	}
	return addColumnVisibility(visibility, out, outError)
}

//export shoal_column_visibility_expression
func shoal_column_visibility_expression(handle *C.shoal_column_visibility, out **C.shoal_bytes_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	value, err := lookupColumnVisibility(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	return allocVisibilityBytes(value.Expression(), out, outError)
}

func visibilityNodeFromColumn(handle *C.shoal_column_visibility, out **C.shoal_visibility_node, outError **C.shoal_error, normalized bool) C.shoal_status {
	if out != nil {
		*out = nil
	}
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_node is required"))
	}
	value, err := lookupColumnVisibility(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	node := value.Tree()
	if normalized {
		node = value.Normalized()
	}
	return addVisibilityNode(node, out, outError)
}

//export shoal_column_visibility_tree
func shoal_column_visibility_tree(handle *C.shoal_column_visibility, out **C.shoal_visibility_node, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return visibilityNodeFromColumn(handle, out, outError, false)
}

//export shoal_column_visibility_normalized
func shoal_column_visibility_normalized(handle *C.shoal_column_visibility, out **C.shoal_visibility_node, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return visibilityNodeFromColumn(handle, out, outError, true)
}

//export shoal_column_visibility_flatten
func shoal_column_visibility_flatten(handle *C.shoal_column_visibility, out **C.shoal_bytes_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	value, err := lookupColumnVisibility(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	return allocVisibilityBytes(value.Flatten(), out, outError)
}

//export shoal_column_visibility_free
func shoal_column_visibility_free(handle **C.shoal_column_visibility) {
	if handle == nil || *handle == nil {
		return
	}
	value := *handle
	*handle = nil
	columnVisibilityValues.remove(uint64(C.shoal_bridge_column_visibility_id(value)))
	C.shoal_bridge_column_visibility_free(value)
}

func sizeToInt(value C.size_t, name string) (int, error) {
	if uint64(value) > uint64(^uint(0)>>1) {
		return 0, fmt.Errorf("shoal: %s exceeds the platform integer range", name)
	}
	return int(value), nil
}

//export shoal_node_expression_create
func shoal_node_expression_create(expression C.shoal_bytes, offset C.size_t, size C.size_t, out **C.shoal_node_expression, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_expression is required"))
	}
	value, err := visibilityBytes(expression, "visibility expression")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	goOffset, err := sizeToInt(offset, "offset")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	goSize, err := sizeToInt(size, "size")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	nodeExpression, err := accumulo.NewNodeExpression(value, goOffset, goSize)
	if err != nil {
		return failForError(outError, err)
	}
	return addNodeExpression(nodeExpression, out, outError)
}

func nodeExpressionBytes(handle *C.shoal_node_expression, out **C.shoal_bytes_result, outError **C.shoal_error, buffer bool) C.shoal_status {
	if out != nil {
		*out = nil
	}
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	value, err := lookupNodeExpression(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	bytes := value.Term()
	if buffer {
		bytes = value.Buffer()
	}
	return allocVisibilityBytes(bytes, out, outError)
}

//export shoal_node_expression_term
func shoal_node_expression_term(handle *C.shoal_node_expression, out **C.shoal_bytes_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return nodeExpressionBytes(handle, out, outError, false)
}

//export shoal_node_expression_buffer
func shoal_node_expression_buffer(handle *C.shoal_node_expression, out **C.shoal_bytes_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	return nodeExpressionBytes(handle, out, outError, true)
}

//export shoal_node_expression_size
func shoal_node_expression_size(handle *C.shoal_node_expression) C.size_t {
	value, err := lookupNodeExpression(handle)
	if err != nil {
		return 0
	}
	return C.size_t(value.Size())
}

//export shoal_node_expression_free
func shoal_node_expression_free(handle **C.shoal_node_expression) {
	if handle == nil || *handle == nil {
		return
	}
	value := *handle
	*handle = nil
	nodeExpressionValues.remove(uint64(C.shoal_bridge_node_expression_id(value)))
	C.shoal_bridge_node_expression_free(value)
}

//export shoal_visibility_node_get
func shoal_visibility_node_get(handle *C.shoal_visibility_node, out *C.shoal_visibility_node_view, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	if out == nil || out.struct_size < C.SHOAL_VISIBILITY_NODE_VIEW_V1_SIZE {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: visibility node view is required and must be initialized"))
	}
	value, err := lookupVisibilityNode(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	out.node_type = C.shoal_visibility_node_type(value.Type())
	out.child_count = C.size_t(value.Size())
	out.span_length = C.size_t(value.Len())
	out.term_start = C.size_t(value.TermStart())
	out.term_end = C.size_t(value.TermEnd())
	out.empty = boolToCUint8(value.Empty())
	return C.SHOAL_STATUS_OK
}

//export shoal_visibility_node_expression
func shoal_visibility_node_expression(handle *C.shoal_visibility_node, out **C.shoal_bytes_result, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_result is required"))
	}
	value, err := lookupVisibilityNode(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	return allocVisibilityBytes(value.Expression(), out, outError)
}

//export shoal_visibility_node_child
func shoal_visibility_node_child(handle *C.shoal_visibility_node, index C.size_t, out **C.shoal_visibility_node, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_child is required"))
	}
	value, err := lookupVisibilityNode(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	goIndex, err := sizeToInt(index, "child index")
	if err != nil || goIndex >= value.Size() {
		if err == nil {
			err = fmt.Errorf("shoal: child index %d is out of range", goIndex)
		}
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	return addVisibilityNode(value.Children()[goIndex], out, outError)
}

//export shoal_visibility_node_term
func shoal_visibility_node_term(handle *C.shoal_visibility_node, expression C.shoal_bytes, out **C.shoal_node_expression, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_expression is required"))
	}
	value, err := lookupVisibilityNode(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	bytes, err := visibilityBytes(expression, "visibility expression")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	if value.Type() != accumulo.VisibilityTerm {
		return failForError(outError, &accumulo.VisibilityParseError{
			Reason: fmt.Sprintf("%s node has no term", value.Type()),
			Terms:  append([]byte(nil), bytes...),
			Offset: value.TermStart(),
		})
	}
	nodeExpression, err := value.Term(bytes)
	if err != nil {
		return failForError(outError, err)
	}
	return addNodeExpression(nodeExpression, out, outError)
}

//export shoal_visibility_node_compare
func shoal_visibility_node_compare(leftHandle *C.shoal_visibility_node, rightHandle *C.shoal_visibility_node, out *C.int32_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = 0
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_comparison is required"))
	}
	left, err := lookupVisibilityNode(leftHandle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	right, err := lookupVisibilityNode(rightHandle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	comparison := accumulo.CompareVisibilityNodes(*left, *right)
	if comparison < 0 {
		*out = -1
	} else if comparison > 0 {
		*out = 1
	}
	return C.SHOAL_STATUS_OK
}

//export shoal_visibility_node_free
func shoal_visibility_node_free(handle **C.shoal_visibility_node) {
	if handle == nil || *handle == nil {
		return
	}
	value := *handle
	*handle = nil
	visibilityNodeValues.remove(uint64(C.shoal_bridge_visibility_node_id(value)))
	C.shoal_bridge_visibility_node_free(value)
}

//export shoal_visibility_evaluator_create
func shoal_visibility_evaluator_create(authorizations *C.shoal_authorizations, out **C.shoal_visibility_evaluator, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_evaluator is required"))
	}
	var auths *accumulo.Authorizations
	var err error
	if authorizations != nil {
		auths, err = lookupAuthorizations(authorizations)
		if err != nil {
			return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
		}
	}
	evaluator := accumulo.NewVisibilityEvaluator(auths)
	id, ok := visibilityEvaluators.add(evaluator)
	if !ok {
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: visibility evaluator handle space exhausted"))
	}
	handle := C.shoal_bridge_visibility_evaluator_alloc(C.uint64_t(id))
	if handle == nil {
		visibilityEvaluators.remove(id)
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate visibility evaluator handle"))
	}
	*out = handle
	return C.SHOAL_STATUS_OK
}

//export shoal_visibility_evaluator_authorizations
func shoal_visibility_evaluator_authorizations(handle *C.shoal_visibility_evaluator, out **C.shoal_authorizations, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = nil
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_authorizations is required"))
	}
	evaluator, err := lookupVisibilityEvaluator(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	auths := evaluator.Authorizations()
	id, ok := authorizationValues.add(auths)
	if !ok {
		return fail(outError, C.SHOAL_STATUS_INTERNAL, errors.New("shoal: authorizations handle space exhausted"))
	}
	authHandle := C.shoal_bridge_authorizations_alloc(C.uint64_t(id))
	if authHandle == nil {
		authorizationValues.remove(id)
		return fail(outError, C.SHOAL_STATUS_OUT_OF_MEMORY, errors.New("shoal: allocate authorizations handle"))
	}
	*out = authHandle
	return C.SHOAL_STATUS_OK
}

//export shoal_visibility_evaluator_set_authorizations
func shoal_visibility_evaluator_set_authorizations(handle *C.shoal_visibility_evaluator, authorizations *C.shoal_authorizations, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	defer recoverStatus(&status, outError)
	evaluator, err := lookupVisibilityEvaluator(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	var auths *accumulo.Authorizations
	if authorizations != nil {
		auths, err = lookupAuthorizations(authorizations)
		if err != nil {
			return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
		}
	}
	evaluator.SetAuthorizations(auths)
	return C.SHOAL_STATUS_OK
}

//export shoal_visibility_evaluator_evaluate
func shoal_visibility_evaluator_evaluate(handle *C.shoal_visibility_evaluator, expression C.shoal_bytes, out *C.uint8_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = 0
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_satisfied is required"))
	}
	evaluator, err := lookupVisibilityEvaluator(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	bytes, err := visibilityBytes(expression, "visibility expression")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	satisfied, err := evaluator.Evaluate(bytes)
	if err != nil {
		return failForError(outError, err)
	}
	*out = boolToCUint8(satisfied)
	return C.SHOAL_STATUS_OK
}

//export shoal_visibility_evaluator_evaluate_tree
func shoal_visibility_evaluator_evaluate_tree(handle *C.shoal_visibility_evaluator, expression C.shoal_bytes, nodeHandle *C.shoal_visibility_node, out *C.uint8_t, outError **C.shoal_error) (status C.shoal_status) {
	clearError(outError)
	if out != nil {
		*out = 0
	}
	defer recoverStatus(&status, outError)
	if out == nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, errors.New("shoal: out_satisfied is required"))
	}
	evaluator, err := lookupVisibilityEvaluator(handle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	node, err := lookupVisibilityNode(nodeHandle)
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_HANDLE, err)
	}
	bytes, err := visibilityBytes(expression, "visibility expression")
	if err != nil {
		return fail(outError, C.SHOAL_STATUS_INVALID_ARGUMENT, err)
	}
	satisfied, err := evaluator.EvaluateTree(bytes, *node)
	if err != nil {
		return failForError(outError, err)
	}
	*out = boolToCUint8(satisfied)
	return C.SHOAL_STATUS_OK
}

//export shoal_visibility_evaluator_free
func shoal_visibility_evaluator_free(handle **C.shoal_visibility_evaluator) {
	if handle == nil || *handle == nil {
		return
	}
	value := *handle
	*handle = nil
	visibilityEvaluators.remove(uint64(C.shoal_bridge_visibility_evaluator_id(value)))
	C.shoal_bridge_visibility_evaluator_free(value)
}
