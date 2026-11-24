package rpcx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/Yulian302/deliverit/platform/rpcx"
)

const procedure = "/test.v1.TestService/Ping"

// serve mounts one unary RPC that returns err (nil = success), wrapped in
// rpcx.AccessLog, on an HTTP/2-capable test server. It returns a client
// factory and the captured log.
func serve(t *testing.T, err error) (func(...connect.ClientOption) *connect.Client[emptypb.Empty, emptypb.Empty], *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))

	mux := http.NewServeMux()
	mux.Handle(procedure, connect.NewUnaryHandler(procedure,
		func(context.Context, *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
			if err != nil {
				return nil, err
			}
			return connect.NewResponse(&emptypb.Empty{}), nil
		},
		connect.WithInterceptors(rpcx.AccessLog(log)),
	))
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true // gRPC needs HTTP/2
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return func(opts ...connect.ClientOption) *connect.Client[emptypb.Empty, emptypb.Empty] {
		return connect.NewClient[emptypb.Empty, emptypb.Empty](srv.Client(), srv.URL+procedure, opts...)
	}, &logs
}

// oneLine parses the single log line written for one call.
func oneLine(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()
	var lines []string
	if s := strings.TrimSpace(logs.String()); s != "" {
		lines = strings.Split(s, "\n")
	}
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want exactly 1: %q", len(lines), logs.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	return rec
}

func TestAccessLog_Success(t *testing.T) {
	client, logs := serve(t, nil)

	if _, err := client().CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{})); err != nil {
		t.Fatal(err)
	}

	rec := oneLine(t, logs)
	if rec["procedure"] != procedure || rec["code"] != "ok" || rec["protocol"] != "connect" {
		t.Errorf("log = %v, want procedure=%s code=ok protocol=connect", rec, procedure)
	}
	if _, ok := rec["duration_ms"]; !ok {
		t.Errorf("no duration_ms: %v", rec)
	}
}

// Over gRPC an error still travels as HTTP 200 (the status is in a trailer),
// so an HTTP access log would record a failure as 200. The RPC log must show
// the real code.
func TestAccessLog_LogsRPCCodeEvenOverGRPC(t *testing.T) {
	client, logs := serve(t, connect.NewError(connect.CodeInvalidArgument, errors.New("bad lat")))

	_, err := client(connect.WithGRPC()).CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("client got %v, want invalid_argument", err)
	}

	rec := oneLine(t, logs)
	if rec["code"] != "invalid_argument" || rec["protocol"] != "grpc" {
		t.Errorf("log = %v, want code=invalid_argument protocol=grpc", rec)
	}
}

// Server errors that aren't Connect errors are reported as "unknown", the
// code the client will see too.
func TestAccessLog_PlainErrorIsUnknown(t *testing.T) {
	client, logs := serve(t, errors.New("boom"))

	_, _ = client().CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{}))

	if rec := oneLine(t, logs); rec["code"] != "unknown" {
		t.Errorf("code = %v, want unknown", rec["code"])
	}
}

// On a client the interceptor must stay quiet: the server already logs the
// call, and a client-side line would double every RPC in the logs.
func TestAccessLog_SilentOnClients(t *testing.T) {
	client, serverLogs := serve(t, nil)
	var clientLogs bytes.Buffer
	clientLog := slog.New(slog.NewJSONHandler(&clientLogs, nil))

	_, err := client(connect.WithInterceptors(rpcx.AccessLog(clientLog))).
		CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{}))
	if err != nil {
		t.Fatal(err)
	}

	if clientLogs.Len() != 0 {
		t.Errorf("client-side interceptor logged: %q", clientLogs.String())
	}
	_ = oneLine(t, serverLogs) // the server still logs exactly once
}
