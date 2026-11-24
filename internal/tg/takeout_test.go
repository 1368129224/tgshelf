package tg

import (
	"context"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func TestInitTakeoutSession_FileMaxSizeInBytes(t *testing.T) {
	var gotReq *tg.AccountInitTakeoutSessionRequest
	inv := asInvoker(func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
		req, ok := input.(*tg.AccountInitTakeoutSessionRequest)
		if !ok {
			t.Fatalf("expected *tg.AccountInitTakeoutSessionRequest, got %T", input)
		}
		gotReq = req
		if out, ok := output.(*tg.AccountTakeout); ok {
			out.ID = 12345
		}
		return nil
	})

	id, err := InitTakeoutSession(context.Background(), tg.NewClient(inv), 4000)
	if err != nil {
		t.Fatalf("InitTakeoutSession: %v", err)
	}
	if id != 12345 {
		t.Fatalf("id = %d, want 12345", id)
	}
	wantBytes := int64(4000) << 20
	if gotReq == nil || gotReq.FileMaxSize != wantBytes {
		t.Fatalf("FileMaxSize = %d, want %d", gotReq.FileMaxSize, wantBytes)
	}
}

func TestFinishTakeoutSession_WrapsInvokeWithTakeout(t *testing.T) {
	var gotWrap *tg.InvokeWithTakeoutRequest
	inv := asInvoker(func(_ context.Context, input bin.Encoder, output bin.Decoder) error {
		wrap, ok := input.(*tg.InvokeWithTakeoutRequest)
		if !ok {
			t.Fatalf("expected *tg.InvokeWithTakeoutRequest, got %T", input)
		}
		gotWrap = wrap
		return nil
	})

	const wantID int64 = 987654321
	if err := FinishTakeoutSession(context.Background(), tg.NewClient(inv), wantID); err != nil {
		t.Fatalf("FinishTakeoutSession: %v", err)
	}
	if gotWrap == nil {
		t.Fatal("expected InvokeWithTakeoutRequest to be invoked")
	}
	if gotWrap.TakeoutID != wantID {
		t.Fatalf("TakeoutID = %d, want %d", gotWrap.TakeoutID, wantID)
	}
	fin, ok := gotWrap.Query.(*tg.AccountFinishTakeoutSessionRequest)
	if !ok {
		t.Fatalf("Query = %T, want *tg.AccountFinishTakeoutSessionRequest", gotWrap.Query)
	}
	if !fin.Success {
		t.Fatalf("expected AccountFinishTakeoutSessionRequest.Success == true")
	}
}
