/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

// Package retrievalgrpc adapts the public retrieval contract to gRPC.
//
// Protobuf does not distinguish nil from empty repeated fields or maps. This
// package normalizes both forms to nil after a wire round trip. Empty scopes
// and zero-value graph paths are omitted; optional message presence such as an
// explanation is preserved.
package retrievalgrpc

import (
	"context"
	"errors"
	"reflect"

	"github.com/phrocker/shoal-oss/internal/knowledgepb"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type server struct {
	knowledgepb.UnimplementedKnowledgeRetrievalServer
	retriever retrieval.Retriever
}

// RegisterServer exposes retriever through the knowledge retrieval service.
func RegisterServer(registrar grpc.ServiceRegistrar, retriever retrieval.Retriever) {
	knowledgepb.RegisterKnowledgeRetrievalServer(registrar, &server{retriever: retriever})
}

func (s *server) Retrieve(
	ctx context.Context, request *knowledgepb.RetrieveRequest,
) (*knowledgepb.RetrieveResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, toStatusError(err)
	}
	if isNilInterface(s.retriever) {
		return nil, status.Error(codes.Internal, "knowledge retriever is not configured")
	}

	publicRequest, err := requestFromProto(request)
	if err != nil {
		return nil, toStatusError(err)
	}
	response, err := s.retriever.Retrieve(ctx, publicRequest)
	if err != nil {
		return nil, toStatusError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, toStatusError(err)
	}

	protoResponse, err := responseToProto(publicRequest, response)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return protoResponse, nil
}

type client struct {
	client knowledgepb.KnowledgeRetrievalClient
}

// NewClient returns a retrieval.Retriever backed by a gRPC connection.
func NewClient(connection grpc.ClientConnInterface) (retrieval.Retriever, error) {
	if isNilInterface(connection) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "knowledge retrieval connection is required")
	}
	return &client{client: knowledgepb.NewKnowledgeRetrievalClient(connection)}, nil
}

func (c *client) Retrieve(
	ctx context.Context, request retrieval.Request,
) (retrieval.Response, error) {
	if c == nil || isNilInterface(c.client) {
		return retrieval.Response{}, shoal.NewError(
			shoal.ErrorInternal, "knowledge retrieval client is not configured")
	}
	if err := ctx.Err(); err != nil {
		return retrieval.Response{}, fromStatusError(toStatusError(err))
	}

	normalized, err := request.Normalize()
	if err != nil {
		return retrieval.Response{}, err
	}
	protoRequest, err := requestToProto(normalized)
	if err != nil {
		return retrieval.Response{}, err
	}
	protoResponse, err := c.client.Retrieve(ctx, protoRequest)
	if err != nil {
		return retrieval.Response{}, fromStatusError(err)
	}

	response, err := responseFromProto(normalized, protoResponse)
	if err != nil {
		return retrieval.Response{}, shoal.WrapError(
			shoal.ErrorInternal, "invalid knowledge retrieval response", err)
	}
	return response, nil
}

func toStatusError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, context.Canceled.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, context.DeadlineExceeded.Error())
	}

	var publicError *shoal.Error
	if !errors.As(err, &publicError) || publicError == nil {
		return status.Error(codes.Internal, "knowledge retrieval failed")
	}

	message := publicError.Message
	if message == "" {
		message = string(publicError.Code)
	}
	return status.Error(grpcCode(publicError.Code), message)
}

func grpcCode(code shoal.ErrorCode) codes.Code {
	switch code {
	case shoal.ErrorInvalidArgument:
		return codes.InvalidArgument
	case shoal.ErrorNotFound:
		return codes.NotFound
	case shoal.ErrorConflict:
		return codes.Aborted
	case shoal.ErrorUnauthorized:
		return codes.PermissionDenied
	case shoal.ErrorUnavailable:
		return codes.Unavailable
	case shoal.ErrorCanceled:
		return codes.Canceled
	case shoal.ErrorDeadline:
		return codes.DeadlineExceeded
	default:
		return codes.Internal
	}
}

func fromStatusError(err error) error {
	if err == nil {
		return nil
	}

	grpcStatus := status.Convert(err)
	var code shoal.ErrorCode
	var cause error
	switch grpcStatus.Code() {
	case codes.InvalidArgument:
		code = shoal.ErrorInvalidArgument
	case codes.NotFound:
		code = shoal.ErrorNotFound
	case codes.AlreadyExists, codes.Aborted:
		code = shoal.ErrorConflict
	case codes.Unauthenticated, codes.PermissionDenied:
		code = shoal.ErrorUnauthorized
	case codes.Unavailable, codes.ResourceExhausted:
		code = shoal.ErrorUnavailable
	case codes.Canceled:
		code = shoal.ErrorCanceled
		cause = context.Canceled
	case codes.DeadlineExceeded:
		code = shoal.ErrorDeadline
		cause = context.DeadlineExceeded
	default:
		code = shoal.ErrorInternal
	}
	if cause == nil {
		cause = err
	}
	return shoal.WrapError(code, grpcStatus.Message(), cause)
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Ptr, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

var (
	_ knowledgepb.KnowledgeRetrievalServer = (*server)(nil)
	_ retrieval.Retriever                  = (*client)(nil)
)
