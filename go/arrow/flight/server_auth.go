// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package flight

import (
	"context"
	"encoding/base64"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	grpcAuthHeader    = "auth-token-bin"
	basicAuthHeader   = "authorization"
	basicAuthPrefix   = "Basic"
	bearerTokenPrefix = "Bearer"
)

// AuthConn wraps the stream from grpc for handshakes to simplify handling
// handshake request and response from the flight.proto forwarding just the
// payloads and errors instead of having to deal with the handshake request
// and response protos directly
type AuthConn interface {
	Read() ([]byte, error)
	Send([]byte) error
}

type serverAuthConn struct {
	stream FlightService_HandshakeServer
}

func (a *serverAuthConn) Read() ([]byte, error) {
	in, err := a.stream.Recv()
	if err != nil {
		return nil, err
	}

	return in.Payload, nil
}

func (a *serverAuthConn) Send(b []byte) error {
	return a.stream.Send(&HandshakeResponse{Payload: b})
}

// ServerAuthHandler defines an interface for the server to perform the handshake.
// The token is expected to be sent as part of the context metadata in subsequent
// requests with a key of "auth-token-bin" which will then call IsValid to validate
type ServerAuthHandler interface {
	Authenticate(AuthConn) error
	IsValid(token string) (interface{}, error)
}

type authCtxKey struct{}

type authWrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (a *authWrappedStream) Context() context.Context { return a.ctx }

// AuthFromContext will return back whatever object was returned from `IsValid` for a
// given request context allowing handlers to retrieve identifying information
// for the current request for use.
func AuthFromContext(ctx context.Context) interface{} {
	return ctx.Value(authCtxKey{})
}

func createServerAuthUnaryInterceptor(auth ServerAuthHandler) grpc.UnaryServerInterceptor {
	if auth == nil {
		return func(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			return handler(ctx, req)
		}
	}

	return func(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		var authTok string
		md, ok := metadata.FromIncomingContext(ctx)
		if ok {
			vals := md.Get(grpcAuthHeader)
			if len(vals) > 0 {
				authTok = vals[0]
			}
		}

		peerIdentity, err := auth.IsValid(authTok)
		if err != nil {
			return nil, status.Errorf(codes.PermissionDenied, "auth-error: %s", err)
		}

		return handler(context.WithValue(ctx, authCtxKey{}, peerIdentity), req)
	}
}

func createServerAuthStreamInterceptor(auth ServerAuthHandler) grpc.StreamServerInterceptor {
	if auth == nil {
		return func(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			return handler(srv, stream)
		}
	}

	return func(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if strings.HasSuffix(info.FullMethod, "/Handshake") {
			return handler(srv, stream)
		}

		var authTok string
		md, ok := metadata.FromIncomingContext(stream.Context())
		if ok {
			vals := md.Get(grpcAuthHeader)
			if len(vals) > 0 {
				authTok = vals[0]
			}
		}

		peerIdentity, err := auth.IsValid(authTok)
		if err != nil {
			return status.Errorf(codes.Unauthenticated, "auth-error: %s", err)
		}

		stream = &authWrappedStream{ServerStream: stream, ctx: context.WithValue(stream.Context(), authCtxKey{}, peerIdentity)}
		return handler(srv, stream)
	}
}

// our implementation of handshake using the authhandler
func (s *server) handshake(stream FlightService_HandshakeServer) error {
	if s.authHandler == nil {
		return nil
	}

	return s.authHandler.Authenticate(&serverAuthConn{stream})
}

type BasicAuthValidator interface {
	Validate(username, password string) (string, error)
	IsValid(bearerToken string) (interface{}, error)
}

func createServerBearerTokenUnaryInterceptor(validator BasicAuthValidator) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		var auth string
		md, ok := metadata.FromIncomingContext(ctx)
		if ok {
			vals := md.Get(basicAuthHeader)
			if len(vals) > 0 && strings.HasPrefix(vals[0], bearerTokenPrefix) {
				auth = vals[0][len(bearerTokenPrefix)+1:]
			}
		}

		identity, err := validator.IsValid(auth)
		if err != nil {
			return nil, err
		}

		return handler(context.WithValue(ctx, authCtxKey{}, identity), req)
	}
}

func createServerBearerTokenStreamInterceptor(validator BasicAuthValidator) grpc.StreamServerInterceptor {
	return func(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		var auth []string
		md, ok := metadata.FromIncomingContext(stream.Context())
		if ok {
			auth = md.Get(basicAuthHeader)
			if len(auth) > 0 {
				auth = strings.Split(auth[0], " ")
			}
		}

		if auth == nil || len(auth) == 0 {
			return status.Error(codes.Unauthenticated, "must authenticate first")
		}

		if strings.HasSuffix(info.FullMethod, "/Handshake") {
			if auth[0] == basicAuthPrefix {
				val, err := base64.RawStdEncoding.DecodeString(auth[1])
				if err != nil {
					return status.Errorf(codes.Unauthenticated, "invalid basic auth encoding: %s", err)
				}

				creds := strings.SplitN(string(val), ":", 2)
				token, err := validator.Validate(creds[0], creds[1])
				if err != nil {
					return err
				}

				stream.SetTrailer(metadata.New(map[string]string{basicAuthHeader: strings.Join([]string{bearerTokenPrefix, token}, " ")}))
				return handler(srv, stream)
			}
			return status.Errorf(codes.Unauthenticated, "only Basic Auth implemented")
		}

		if auth[0] == bearerTokenPrefix {
			identity, err := validator.IsValid(auth[1])
			if err != nil {
				return err
			}
			return handler(srv, &authWrappedStream{ServerStream: stream, ctx: context.WithValue(stream.Context(), authCtxKey{}, identity)})
		}
		return status.Errorf(codes.Unauthenticated, "Only bearer token auth implemented")
	}
}

func CreateServerBearerTokenAuthInterceptors(validator BasicAuthValidator) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor) {
	if validator == nil {
		panic("validator cannot be nil")
	}

	return createServerBearerTokenUnaryInterceptor(validator), createServerBearerTokenStreamInterceptor(validator)
}
