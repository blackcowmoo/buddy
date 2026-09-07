package main

import (
	"context"
	"testing"
	"time"

	"buddy/server/internal/asyncjob"
)

func TestStartWorkerReturnsNilWhenRedisIsDisabled(t *testing.T) {
	called := false
	queue := startWorker(nil, context.Background(), asyncjob.KindReply, 1, time.Minute,
		func(context.Context, asyncjob.Job) error {
			called = true
			return nil
		})

	if queue != nil {
		t.Fatal("startWorker() returned a queue without Redis")
	}
	if called {
		t.Fatal("startWorker() started a handler without Redis")
	}
}
