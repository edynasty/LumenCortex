package session

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// DATA §6 CR-AJ-1: an auto_effort request names its input by one stored row or by the digest of
// input not stored yet, never both, and RequestsExposing finds a row through any of the envelope's
// pointers.

func judgeAdmission(sessionID, requestID string) RequestAdmission {
	return RequestAdmission{RequestID: requestID, SessionID: sessionID, RunID: "run1", Purpose: PurposeAutoEffort, LineageComplete: true}
}

const digestA = "sha256:" + "aa00000000000000000000000000000000000000000000000000000000000000"

func TestAutoEffortAdmissionNamesItsInputOnce(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")

	stored := judgeAdmission("s", "stored")
	stored.FirstSeq, stored.LastSeq = i64(3), i64(3)
	pending := judgeAdmission("s", "pending")
	pending.PrevUserSeq, pending.LastAssistantSeq, pending.PendingInputDigest = i64(3), i64(4), digestA
	none := judgeAdmission("s", "none")
	none.PrevUserSeq, none.LastAssistantSeq = i64(3), i64(4)
	for _, a := range []RequestAdmission{stored, pending, none} {
		admit(t, store, a)
	}

	for name, a := range map[string]RequestAdmission{
		"row and digest": func() RequestAdmission {
			a := judgeAdmission("s", "bad")
			a.FirstSeq, a.LastSeq, a.PendingInputDigest = i64(3), i64(3), digestA
			return a
		}(),
		"a window, not one row": func() RequestAdmission {
			a := judgeAdmission("s", "bad")
			a.FirstSeq, a.LastSeq = i64(2), i64(3)
			return a
		}(),
	} {
		if _, err := store.AdmitRequest(ctx, a); err == nil || !strings.Contains(err.Error(), "auto_effort") {
			t.Errorf("%s: %v, want an auto_effort refusal", name, err)
		}
	}
	if n := count(t, store, `SELECT count(*) FROM request_envelopes`); n != 3 {
		t.Errorf("%d envelopes after the refusals, want 3", n)
	}
}

func TestRequestsExposingFindsTheRowThroughEveryPointer(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	gapSession(t, store, "other", "idle", "")

	stored := judgeAdmission("s", "a-stored")
	stored.FirstSeq, stored.LastSeq = i64(0), i64(0)
	admit(t, store, stored)
	main := mainAdmission("s", "b-main") // window 0..4
	admit(t, store, main)
	pending := judgeAdmission("s", "c-pending")
	pending.PrevUserSeq, pending.LastAssistantSeq, pending.PendingInputDigest = i64(0), i64(1), digestA
	admit(t, store, pending)
	cont := judgeAdmission("s", "d-cont")
	cont.PrevUserSeq, cont.LastAssistantSeq = i64(5), i64(6)
	admit(t, store, cont)
	foreign := judgeAdmission("other", "e-foreign")
	foreign.FirstSeq, foreign.LastSeq = i64(5), i64(5)
	admit(t, store, foreign)
	// A request refused before it was sent exposes nothing.
	failed := judgeAdmission("s", "f-failed")
	failed.FirstSeq, failed.LastSeq = i64(5), i64(5)
	admit(t, store, failed)
	if err := store.SettleRequest(ctx, "f-failed", Settlement{To: RequestFailed, Reason: "test"}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		seq    int64
		digest string
		want   []string
	}{
		{"stored input row, main window, pending judge's context", 0, "", []string{"a-stored", "b-main", "c-pending"}},
		{"last assistant seen by the pending judge", 1, "", []string{"b-main", "c-pending"}},
		{"pending input stored later at seq 7", 7, digestA, []string{"c-pending"}},
		{"continuation context; failed and foreign excluded", 5, "", []string{"d-cont"}},
		{"nothing", 9, "", nil},
	} {
		got, err := store.RequestsExposing(ctx, "s", tc.seq, tc.digest)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	if _, err := store.RequestsExposing(ctx, "s", 0, "not a digest"); err == nil {
		t.Error("a malformed digest was accepted")
	}
}
