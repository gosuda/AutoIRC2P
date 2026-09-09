package store

import (
	"context"
	"errors"
	"maps"
	"math"
	"path/filepath"
	"testing"
)

func roomSummaryFixture(t *testing.T) *Queries {
	t.Helper()
	db, q, err := NewSQLite(t.Context(), filepath.Join(t.TempDir(), "rooms.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	_, err = db.ExecContext(t.Context(), `
INSERT INTO messages(id,room,nick,original,service,created_at,sender_user_id) VALUES
 (10,'#one','peer','first',0,1000,0),
 (20,'#one','viewer','own message',0,1000,7),
 (25,'#two','peer','other room',0,1000,0),
 (30,'#one','service','joined',1,1000,0),
 (40,'#one','other','registered peer',0,1000,8),
 (50,'#one','peer','remote peer',0,1000,0),
 (60,'#one','service','left',1,1000,0),
 (70,'#two','viewer','own latest message',0,1000,7);`)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestRoomSummariesBoundsCursorBeforeCountingUnread(t *testing.T) {
	q := roomSummaryFixture(t)
	for _, tc := range []struct {
		name       string
		cursor     int64
		hasCursor  bool
		userID     int64
		wantCursor int64
		wantUnread int64
	}{
		{name: "absent cursor starts at latest", userID: 7, wantCursor: 60},
		{name: "zero cursor excludes service and own", hasCursor: true, userID: 7, wantUnread: 3},
		{name: "cursor before room history", cursor: 9, hasCursor: true, userID: 7, wantUnread: 3},
		{name: "cursor in another room bounds to own message", cursor: 25, hasCursor: true, userID: 7, wantCursor: 20, wantUnread: 2},
		{name: "service message remains a valid cursor", cursor: 30, hasCursor: true, userID: 7, wantCursor: 30, wantUnread: 2},
		{name: "future cursor bounds to latest", cursor: math.MaxInt64, hasCursor: true, userID: 7, wantCursor: 60},
		{name: "guest counts all nonservice messages", hasCursor: true, wantUnread: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cursors := make(map[string]int64)
			if tc.hasCursor {
				cursors["#one"] = tc.cursor
			}
			before := maps.Clone(cursors)
			got, err := q.RoomSummaries(t.Context(), []string{"#one"}, cursors, tc.userID)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("summary count = %d, want 1", len(got))
			}
			if got[0].Name != "#one" || got[0].LatestMessageID != 60 || got[0].Cursor != tc.wantCursor || got[0].UnreadCount != tc.wantUnread {
				t.Fatalf("summary = %+v, want #one latest=60 cursor=%d unread=%d", got[0], tc.wantCursor, tc.wantUnread)
			}
			if !maps.Equal(cursors, before) {
				t.Fatalf("input cursors changed: got %v, want %v", cursors, before)
			}
		})
	}
}

func TestRoomSummariesPreservesRequestedRoomsAndOrder(t *testing.T) {
	q := roomSummaryFixture(t)
	cursors := map[string]int64{"#one": 25, "#two": 0, "#empty": math.MaxInt64}
	got, err := q.RoomSummaries(t.Context(), []string{"#two", "#empty", "#one", "#two"}, cursors, 7)
	if err != nil {
		t.Fatal(err)
	}
	want := []RoomSummary{
		{Name: "#two", LatestMessageID: 70, Cursor: 0, UnreadCount: 1},
		{Name: "#empty", LatestMessageID: 0, Cursor: 0, UnreadCount: 0},
		{Name: "#one", LatestMessageID: 60, Cursor: 20, UnreadCount: 2},
		{Name: "#two", LatestMessageID: 70, Cursor: 0, UnreadCount: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("summary count = %d, want %d", len(got), len(want))
	}
	for i, expected := range want {
		if got[i] != expected {
			t.Errorf("summary %d = %+v, want %+v", i, got[i], expected)
		}
	}
	got, err = q.RoomSummaries(t.Context(), []string{"#one", "#empty"}, nil, 7)
	if err != nil || len(got) != 2 {
		t.Fatalf("nil cursors: summaries=%v err=%v", got, err)
	}
	if got[0].Cursor != 60 || got[0].UnreadCount != 0 || got[1].Cursor != 0 || got[1].UnreadCount != 0 {
		t.Fatalf("rooms without cursors contain unread history: %+v", got)
	}
	got, err = q.RoomSummaries(t.Context(), nil, cursors, 7)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty request: summaries=%v err=%v", got, err)
	}
}

func TestRoomSummariesCancellationLeavesCursorsUnchanged(t *testing.T) {
	q := roomSummaryFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cursors := map[string]int64{"#one": math.MaxInt64}
	got, err := q.RoomSummaries(ctx, []string{"#one", "#two"}, cursors, 7)
	if !errors.Is(err, context.Canceled) || len(got) != 0 {
		t.Fatalf("cancelled summary: summaries=%v err=%v", got, err)
	}
	if len(cursors) != 1 || cursors["#one"] != math.MaxInt64 {
		t.Fatalf("cancelled query changed input cursors: %v", cursors)
	}
}
