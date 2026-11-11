package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// After a resume fails, a message names that failure rather than only saying
// there is no thread, so the person sees why the session cannot go on.
func TestHandleMessageAfterAFailedResumeNamesTheResumeFailure(t *testing.T) {
	bridge := &Bridge{resumeFailure: errors.New("thread/resume: already has an active writer")}
	err := bridge.HandleMessage(context.Background(), "hello")
	if err == nil || !strings.Contains(err.Error(), "resuming the conversation failed") || !strings.Contains(err.Error(), "already has an active writer") {
		t.Fatalf("err = %v, want one naming the failed resume", err)
	}
}

// With no failed resume, the error stays the plain one.
func TestHandleMessageWithNoThreadAndNoResumeFailure(t *testing.T) {
	err := (&Bridge{}).HandleMessage(context.Background(), "hello")
	if err == nil || err.Error() != "no active thread" {
		t.Fatalf("err = %v, want \"no active thread\"", err)
	}
}
