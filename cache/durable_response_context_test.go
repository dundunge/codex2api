package cache

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func openTestDurableResponseContextStore(t *testing.T, maxBytes int64) *DurableResponseContextStore {
	t.Helper()
	store, err := OpenDurableResponseContextStore(filepath.Join(t.TempDir(), "contexts.db"), DurableResponseContextOptions{
		TTL: time.Hour, MaxBytes: maxBytes, SweepInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func TestDurableResponseContextExpirationPreservesRenewedCandidates(t *testing.T) {
	for _, renewal := range []string{"rewrite", "read_touch"} {
		t.Run(renewal, func(t *testing.T) {
			ctx := context.Background()
			store := openTestDurableResponseContextStore(t, 0)
			shared := json.RawMessage(`{"role":"user","content":"shared"}`)
			old := json.RawMessage(`{"role":"assistant","content":"old"}`)
			want := []json.RawMessage{shared, old}
			for _, key := range []string{"renewed", "expired", "retained"} {
				items := []json.RawMessage{shared}
				if key == "renewed" {
					items = want
				}
				if err := store.SetResponseContext(ctx, key, items, time.Hour); err != nil {
					t.Fatal(err)
				}
			}
			if renewal == "read_touch" {
				if _, err := store.GetResponseContext(ctx, "renewed"); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now()
			if _, err := store.db.ExecContext(ctx, `UPDATE response_contexts SET expires_at = ? WHERE key IN ('renewed', 'expired')`, now.Add(-time.Minute).UnixMilli()); err != nil {
				t.Fatal(err)
			}
			keys, err := store.selectKeys(ctx, `SELECT key FROM response_contexts WHERE expires_at <= ?`, now.UnixMilli())
			if err != nil || len(keys) != 2 {
				t.Fatalf("expiration candidates=%v err=%v", keys, err)
			}
			// Renew after selection to reproduce a stale expiration candidate.
			if renewal == "rewrite" {
				want = []json.RawMessage{shared, json.RawMessage(`{"role":"assistant","content":"new"}`)}
				if err := store.SetResponseContext(ctx, "renewed", want, time.Hour); err != nil {
					t.Fatal(err)
				}
			} else {
				store.flushTouches()
			}
			deleted, err := store.deleteExpiredBatch(ctx, keys, now)
			if err != nil || deleted != 1 {
				t.Fatalf("expired deletions=%d err=%v, want only the still-expired row", deleted, err)
			}
			got, err := store.GetResponseContext(ctx, "renewed")
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("renewed context=%s err=%v, want %s", got, err, want)
			}
			got, err = store.GetResponseContext(ctx, "retained")
			if err != nil || !reflect.DeepEqual(got, []json.RawMessage{shared}) {
				t.Fatalf("shared retained context=%s err=%v", got, err)
			}
			got, err = store.GetResponseContext(ctx, "expired")
			if err != nil || got != nil {
				t.Fatalf("expired context=%s err=%v, want removed", got, err)
			}
			stats, err := store.Stats(ctx)
			if err != nil || stats.Contexts != 2 || stats.Items != 2 || stats.ItemBytes != int64(len(shared)+len(want[1])) {
				t.Fatalf("retained stats=%+v err=%v", stats, err)
			}
			hash := durableResponseContextHash(shared)
			var refs int
			if err := store.db.QueryRowContext(ctx, `SELECT refs FROM response_context_items WHERE hash = ?`, hash[:]).Scan(&refs); err != nil || refs != 2 {
				t.Fatalf("shared refs=%d err=%v, want 2", refs, err)
			}
			if expired, evicted, err := store.Sweep(now); err != nil || expired != 0 || evicted != 0 {
				t.Fatalf("sweep after renewal: expired=%d evicted=%d err=%v", expired, evicted, err)
			}
			// Explicit invalidation must still remove a live context and its refs.
			if err := store.SetResponseContext(ctx, "renewed", nil, time.Hour); err != nil {
				t.Fatal(err)
			}
			stats, err = store.Stats(ctx)
			if err != nil || stats.Contexts != 1 || stats.Items != 1 || stats.ItemBytes != int64(len(shared)) {
				t.Fatalf("stats after explicit deletion=%+v err=%v", stats, err)
			}
		})
	}
}

func TestDurableResponseContextSweepEvictsLiveContextsOverBudget(t *testing.T) {
	ctx := context.Background()
	store := openTestDurableResponseContextStore(t, 64)
	for _, key := range []string{"a", "b"} {
		item := json.RawMessage(`{"text":"` + strings.Repeat(key, 32) + `"}`)
		if err := store.SetResponseContext(ctx, key, []json.RawMessage{item}, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	expired, evicted, err := store.Sweep(time.Now())
	if err != nil || expired != 0 || evicted != 1 {
		t.Fatalf("sweep: expired=%d evicted=%d err=%v", expired, evicted, err)
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats.Contexts != 1 || stats.Items != 1 || stats.ItemBytes > store.MaxBytes() {
		t.Fatalf("stats after capacity eviction=%+v err=%v", stats, err)
	}
}
