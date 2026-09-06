package store

import (
	"context"
	"errors"
	"time"
)

var errRetentionPolicy = errors.New("invalid retention policy: durations must be nonnegative, send payload retention at least one hour, batch size between 1 and 10000")

type RetentionPolicy struct {
	Messages     time.Duration
	Translations time.Duration
	SendPayloads time.Duration
	BatchSize    int
}

type RetentionResult struct {
	MessagesDeleted     int64
	TranslationsDeleted int64
	SendPayloadsPurged  int64
}

// Prune commits each bounded batch independently; counts include only committed work.
// Payload erasure is logical: existing WAL pages and backups may retain old bytes.
func (q *Queries) Prune(ctx context.Context, now time.Time, policy RetentionPolicy) (RetentionResult, error) {
	var result RetentionResult
	if policy.BatchSize == 0 {
		policy.BatchSize = 500
	}
	negativeAge := policy.Messages < 0 || policy.Translations < 0 || policy.SendPayloads < 0
	shortEchoWindow := policy.SendPayloads > 0 && policy.SendPayloads < time.Hour
	invalidBatch := policy.BatchSize < 1 || policy.BatchSize > 10000
	if negativeAge || shortEchoWindow || invalidBatch {
		return result, errRetentionPolicy
	}
	limit := int64(policy.BatchSize)
	categories := []struct {
		age   time.Duration
		total *int64
		apply func(context.Context, *Queries, int64) (int64, error)
	}{
		{policy.Messages, &result.MessagesDeleted, func(ctx context.Context, tx *Queries, cutoff int64) (int64, error) {
			return tx.PruneMessagesBatch(ctx, PruneMessagesBatchParams{CreatedAt: cutoff, Limit: limit})
		}},
		{policy.Translations, &result.TranslationsDeleted, func(ctx context.Context, tx *Queries, cutoff int64) (int64, error) {
			return tx.PruneTranslationsBatch(ctx, PruneTranslationsBatchParams{CreatedAt: cutoff, Limit: limit})
		}},
		{policy.SendPayloads, &result.SendPayloadsPurged, func(ctx context.Context, tx *Queries, cutoff int64) (int64, error) {
			return tx.PurgeSendPayloadsBatch(ctx, PurgeSendPayloadsBatchParams{UpdatedAt: cutoff, CreatedAt: cutoff, Limit: limit})
		}},
	}
	for _, category := range categories {
		if category.age == 0 {
			continue
		}
		cutoff := now.Add(-category.age).UnixMilli()
		for {
			if err := ctx.Err(); err != nil {
				return result, err
			}
			var count int64
			err := q.Transaction(ctx, func(tx *Queries) error {
				var err error
				count, err = category.apply(ctx, tx, cutoff)
				return err
			})
			if err != nil {
				return result, err
			}
			*category.total += count
			if count < limit {
				break
			}
		}
	}
	return result, ctx.Err()
}
