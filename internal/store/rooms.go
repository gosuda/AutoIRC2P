package store

import (
	"context"
	"encoding/json"
)

type RoomSummary = SelectRoomSummariesRow

// RoomSummaries bounds supplied cursors to stored messages without modifying cursors.
// Rooms without a cursor start at their latest message with no unread history.
func (q *Queries) RoomSummaries(ctx context.Context, names []string, cursors map[string]int64, userID int64) ([]RoomSummary, error) {
	if len(names) == 0 {
		return []RoomSummary{}, nil
	}
	requested := make([]struct {
		Name      string `json:"name"`
		Cursor    int64  `json:"cursor"`
		HasCursor bool   `json:"hasCursor"`
	}, len(names))
	for i, name := range names {
		requested[i].Name = name
		requested[i].Cursor, requested[i].HasCursor = cursors[name]
	}
	encoded, err := json.Marshal(requested)
	if err != nil {
		return nil, err
	}
	return q.SelectRoomSummaries(ctx, SelectRoomSummariesParams{Rooms: string(encoded), UserID: userID})
}
