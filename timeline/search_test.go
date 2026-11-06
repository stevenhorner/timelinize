package timeline

import (
	"context"
	"slices"
	"testing"
	"time"
)

// newSearchTestTimeline creates a timeline with a small, hand-built graph that
// exercises the parts of the search query that join other tables: entities
// with duplicate entity_attributes rows, items with several embeddings, and
// items with incoming relationships of various kinds.
func newSearchTestTimeline(t *testing.T) *Timeline {
	t.Helper()
	ctx := context.Background()

	tl, err := Create(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("creating timeline: %v", err)
	}
	t.Cleanup(func() { _ = tl.Close() })

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := tl.db.WritePool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}

	exec(`INSERT OR IGNORE INTO entity_types (id, name) VALUES (1, 'person')`)
	exec(`INSERT INTO data_sources (id, name, title) VALUES (9001, 'test_ds_a', 'A'), (9002, 'test_ds_b', 'B')`)
	exec(`INSERT INTO entities (id, type_id, name) VALUES (101, 1, 'Alice'), (102, 1, 'Bob'), (103, 1, 'Carol')`)
	exec(`INSERT INTO attributes (id, name, value) VALUES (201, 'phone', '+1111'), (202, 'phone', '+2222'), (203, 'phone', '+3333')`)
	// attribute 201 belongs to Alice twice (identity on two data sources): this
	// used to produce duplicate result rows that only GROUP BY removed
	exec(`INSERT INTO entity_attributes (entity_id, attribute_id, data_source_id) VALUES
		(101, 201, 9001), (101, 201, 9002), (102, 202, NULL), (103, 203, NULL)`)

	exec(`INSERT INTO relations (id, label, directed, subordinating) VALUES
		(301, 'attachment', 1, 1), (302, 'motion', 1, 1), (303, 'reply', 1, 0), (304, 'sent', 1, 0)`)

	// id, attribute, timestamp, timespan, hidden
	items := []struct {
		id       int64
		attr     any
		ts       int64
		timespan any
		hidden   any
	}{
		{1, 201, 1000, nil, nil},  // message from Alice
		{2, 201, 1001, nil, nil},  // attachment of 1 (not a root item)
		{3, 202, 2000, nil, nil},  // image from Bob
		{4, 202, 2001, nil, nil},  // motion video of 3 (not a root item)
		{5, 202, 3000, nil, nil},  // message from Bob, sent to Carol
		{6, 201, 3500, nil, nil},  // reply to 5 (so 5 has a non-subordinating incoming relation)
		{7, nil, 4000, nil, nil},  // item with two embeddings
		{8, 201, 5000, nil, nil},  // attachment of 1 AND target of a reply from 6
		{9, 201, 6000, nil, 1},    // hidden
		{10, nil, 500, 4500, nil}, // starts early but spans into later times
	}
	for _, it := range items {
		exec(`INSERT INTO items (id, data_source_id, attribute_id, timestamp, timespan, hidden) VALUES (?, 9001, ?, ?, ?, ?)`,
			it.id, it.attr, it.ts, it.timespan, it.hidden)
	}

	exec(`INSERT INTO relationships (relation_id, from_item_id, to_item_id, to_attribute_id) VALUES
		(301, 1, 2, NULL),
		(302, 3, 4, NULL),
		(303, 6, 5, NULL),
		(304, 5, NULL, 203),
		(301, 1, 8, NULL),
		(303, 6, 8, NULL)`)

	exec(`INSERT INTO embeddings (item_id, embedding) VALUES (7, x'00'), (7, x'01')`)

	return tl
}

func TestSearchResultRows(t *testing.T) {
	tl := newSearchTestTimeline(t)
	ctx := context.Background()

	ms := func(v int64) *time.Time { ts := time.UnixMilli(v); return &ts }

	for i, tc := range []struct {
		name   string
		params ItemSearchParams
		expect []uint64
	}{
		{
			name:   "root items only, newest first",
			params: ItemSearchParams{},
			expect: []uint64{8, 7, 6, 5, 3, 1, 10},
		},
		{
			name:   "ascending",
			params: ItemSearchParams{Sort: SortAsc},
			expect: []uint64{10, 1, 3, 5, 6, 7, 8},
		},
		{
			name:   "astructured includes attachments and motion items",
			params: ItemSearchParams{Astructured: true, Sort: SortAsc},
			expect: []uint64{10, 1, 2, 3, 4, 5, 6, 7, 8},
		},
		{
			name: "exclude motion relation (as the timeline page does)",
			params: ItemSearchParams{
				Sort:      SortAsc,
				Relations: []RelationParams{{Not: true, RelationLabel: "motion"}},
			},
			expect: []uint64{10, 1, 3, 5, 6, 7, 8},
		},
		{
			name: "only items at the end of an attachment relation",
			params: ItemSearchParams{
				Astructured: true,
				Sort:        SortAsc,
				Relations:   []RelationParams{{RelationLabel: "attachment"}},
			},
			expect: []uint64{2, 8},
		},
		{
			name:   "entity filter with duplicate entity_attributes rows",
			params: ItemSearchParams{EntityID: []uint64{101}, Sort: SortAsc},
			expect: []uint64{1, 6, 8},
		},
		{
			name:   "items sent to an attribute",
			params: ItemSearchParams{ToAttributeID: []int64{203}},
			expect: []uint64{5},
		},
		{
			name:   "items sent to an entity",
			params: ItemSearchParams{ToEntityID: []int64{103}},
			expect: []uint64{5},
		},
		{
			name: "time range includes items whose timespan reaches into it",
			params: ItemSearchParams{
				StartTimestamp: ms(2000),
				EndTimestamp:   ms(4000),
				Inclusive:      true,
				Sort:           SortAsc,
			},
			expect: []uint64{10, 3, 5, 6, 7},
		},
		{
			name:   "nearest to timestamp",
			params: ItemSearchParams{Timestamp: ms(3400), Limit: 3},
			expect: []uint64{6, 5, 7},
		},
		{
			name:   "limit and offset",
			params: ItemSearchParams{Sort: SortAsc, Limit: 2, Offset: 2},
			expect: []uint64{3, 5},
		},
	} {
		params := tc.params
		params.WithTotal = true
		results, err := tl.Search(ctx, params)
		if err != nil {
			t.Errorf("test %d (%s): unexpected error: %v", i, tc.name, err)
			continue
		}
		var got []uint64
		for _, r := range results.Items {
			got = append(got, r.ID)
		}
		if !slices.Equal(got, tc.expect) {
			t.Errorf("test %d (%s): expected items %v, got %v", i, tc.name, tc.expect, got)
		}

		// the total must count distinct items, not joined rows
		// (with a limit/offset, the total is of all matching items)
		if params.Limit == 0 && results.Total != nil && *results.Total != len(tc.expect) {
			t.Errorf("test %d (%s): expected total %d, got %d", i, tc.name, len(tc.expect), *results.Total)
		}

		// count-only mode should agree
		if params.Limit == 0 {
			params.WithTotal = false
			params.OnlyTotal = true
			count, err := tl.Search(ctx, params)
			if err != nil {
				t.Errorf("test %d (%s): unexpected error counting: %v", i, tc.name, err)
				continue
			}
			if *count.Total != len(tc.expect) {
				t.Errorf("test %d (%s): expected count %d, got %d", i, tc.name, len(tc.expect), *count.Total)
			}
		}
	}
}

func TestSearchResultDetails(t *testing.T) {
	tl := newSearchTestTimeline(t)
	ctx := context.Background()

	results, err := tl.Search(ctx, ItemSearchParams{Astructured: true, Sort: SortAsc})
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[uint64]*SearchResult)
	for _, r := range results.Items {
		byID[r.ID] = r
	}

	expectEntity := map[uint64]uint64{1: 101, 2: 101, 3: 102, 5: 102, 6: 101, 8: 101}
	for itemID, entityID := range expectEntity {
		r := byID[itemID]
		if r == nil || r.Entity == nil || r.Entity.ID == nil || *r.Entity.ID != entityID {
			t.Errorf("item %d: expected entity %d, got %+v", itemID, entityID, r)
		}
	}
	for _, itemID := range []uint64{7, 10} {
		if r := byID[itemID]; r == nil || r.Entity != nil {
			t.Errorf("item %d: expected no entity, got %+v", itemID, r)
		}
	}
	if !byID[7].HasEmbedding {
		t.Errorf("item 7: expected HasEmbedding")
	}
	if byID[1].HasEmbedding {
		t.Errorf("item 1: expected no embedding")
	}
}

func TestSearchQueryPlanUsesIndexes(t *testing.T) {
	tl := newSearchTestTimeline(t)
	ctx := context.Background()

	start, end := time.UnixMilli(1000), time.UnixMilli(2000)
	q, args, err := tl.prepareSearchQuery(ctx, ItemSearchParams{
		StartTimestamp: &start,
		EndTimestamp:   &end,
		Sort:           SortAsc,
		Relations:      []RelationParams{{Not: true, RelationLabel: "motion"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := tl.db.ReadPool.QueryContext(ctx, "EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	for _, step := range plan {
		if step == "SCAN items" || step == "SCAN relationships" {
			t.Errorf("query plan contains a full table scan (%q):\n%v", step, plan)
		}
	}
}
