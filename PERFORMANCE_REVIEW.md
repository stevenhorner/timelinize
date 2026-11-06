# Timelinize: performance and maintenance review

Reviewed at commit `11ecc5a` (May 2026). Focus: why the app is slow on a server with no modern GPU, and why the **timeline view is slow even after processing has finished**.

## TL;DR: highest-impact fixes

| # | Area | Problem | Fix | Effort |
|---|------|---------|-----|--------|
| 1 | DB | No index on `relationships.to_item_id`. Every timeline load runs one **full table scan of `relationships` per result** (up to 2,000 results) | `CREATE INDEX idx_relationships_to_item_id ON relationships(to_item_id);` | Trivial |
| 2 | DB | `GROUP BY items.id` in the search query stops SQLite using `idx_items_timestamp`, so **every timeline query scans the whole `items` table** and then sorts it | Remove the `GROUP BY` by turning the relationship joins into `EXISTS`/`NOT EXISTS` subqueries | Small to medium |
| 3 | Frontend | The timeline asks for `limit: 2000` items with `related: 1` and renders them all into the DOM at once (no pagination or virtualisation) | Load about 100–200 items, then load more as the user scrolls (infinite scroll) | Medium |
| 4 | Frontend | Mapbox GL uses the heavy `standard` style with `antialias: true`, dynamic lighting and terrain. **Without a GPU the browser renders WebGL in software**, which is extremely slow | Use a lighter 2D style, `antialias: false`, no terrain or lighting, and a static map image for the small timeline maps | Small |
| 5 | Thumbnails | Image thumbnails are **AVIF** (slow to encode on the CPU and slow for browsers to decode without hardware support). Missing thumbnails are generated **on demand while you browse** | Offer WebP/JPEG thumbnails as an option; make sure the thumbnail job has finished | Small |
| 6 | HTTP | No compression, and no cache headers on static JS/CSS (about 3.5 MB of JS) or on thumbnails | Add gzip/zstd for JSON/JS/CSS, and `Cache-Control` + `ETag` on thumbnails and vendored assets | Small |

> **Status:** items 1 and 2 are now fixed (see "Changes made" below).

Items 1 and 2 are probably the main cause of "viewing the timeline is very slow", and they have nothing to do with the graphics card. Item 4 is the part that is specific to having no GPU.

## Changes made

**`relationships(to_item_id)` index + removal of `GROUP BY items.id`** (`timeline/schema.sql`, `timeline/search.go`):

* Added `idx_relationships_to_item_id`. `schema.sql` runs every time a timeline is opened, so existing timelines get the index on their next start (a one-time build that can take seconds to minutes on a large library).
* The search query no longer joins `relationships`, `relations` or `embeddings`, and no longer uses `GROUP BY items.id`:
  * the "root items only" and relation-label filters (for example "not motion") became one `EXISTS` / `NOT EXISTS` subquery on the item's incoming relationships, with the same matching rules as before;
  * "has embedding" became a scalar subquery;
  * the entity is chosen with a subquery that picks one `entity_attributes` link (preferring an entity that is being filtered for). An attribute linked to the same entity on several data sources previously produced duplicate rows, which only `GROUP BY` removed.
* Fixed while in there: `to_attribute_id` / `to_entity_id` searches ("items sent to X") always failed. Combined with the default root-items mode they gave `no such column: relations.directed`, and `to_entity_id` referenced a column that doesn't exist. They now work.
* New tests in `timeline/search_test.go` cover result rows, ordering, totals, entities, embeddings and the query plan (no full scans of `items`/`relationships`). Every case except the two broken "sent to" filters gives the same results on the old code.

Measured on a synthetic timeline of 200,000 items and 150,000 relationships:

| Query | Before | After |
|---|---|---|
| Timeline: one day, `related=1` (72 items) | 550 ms | 28 ms |
| One week, `related=1` (504 items) | 3,448 ms | 123 ms |
| Newest 100 items, `related=1` | 1,094 ms | 22 ms |
| "Find latest item" pre-search (`limit 1`) | 323 ms | ~350 ms (not fixed, see 1.6) |

---

## 1. Database / query performance (server side)

I loaded `timeline/schema.sql` into SQLite 3.53 and ran `EXPLAIN QUERY PLAN` on the queries the timeline page actually sends (with representative `sqlite_stat1` row counts).

### 1.1 Missing index: `relationships.to_item_id` (critical) — **FIXED**

`schema.sql` indexes only `relationships.from_item_id`. Two hot queries filter on `to_item_id`:

* `prepareSearchQuery`: `LEFT JOIN relationships ON relationships.to_item_id = items.id` (used to exclude attachments and motion photos from the top level). SQLite currently builds a temporary **automatic index** for this on *every* query.
* `expandRelationshipSingle` (`timeline/search.go:849`): `WHERE relationships.from_item_id=? OR relationships.to_item_id=?`. Current plan:

  ```
  SEARCH items USING INTEGER PRIMARY KEY (rowid=?)
  SCAN relationships          <-- full table scan
  ```

  This query runs **once for every item in the results** (the timeline asks for up to 2,000), and again for related items. That is roughly 2,000 full scans of what is usually the largest table, for each page view.

Plan after adding the index:

```
MULTI-INDEX OR
  SEARCH relationships USING INDEX idx_relationships_from_item_id (from_item_id=?)
  SEARCH relationships USING INDEX idx_relationships_to_item_id (to_item_id=?)
```

**Fix** (add to `schema.sql`; `IF NOT EXISTS` makes it safe on existing databases):

```sql
CREATE INDEX IF NOT EXISTS "idx_relationships_to_item_id" ON "relationships"("to_item_id");
```

This index also speeds up `ON DELETE CASCADE` on `relationships.to_item_id`: SQLite has to find child rows on every item delete, and without the index that means a table scan per deleted item.

Also consider these, depending on which filters are used:

* `relationships(to_attribute_id)`, used by "items sent to person X" (`ToAttributeID`/`ToEntityID`) and by the attribute FK cascade.
* `relationships(from_attribute_id)`, used by the FK cascade when attributes are deleted.
* `items(attribute_id)`, used for the entity page ("items by this person") and the attribute FK.
* `items(classification_id)`, used by the "item class" filter on the timeline.

### 1.2 `GROUP BY items.id` disables the timestamp index (critical) — **FIXED**

Every non-count search ends with `GROUP BY items.id` (`search.go`, just before `ORDER BY`). It exists only to remove duplicate rows produced by the `LEFT JOIN relationships`. The side effect is that SQLite walks `items` in rowid order to do the grouping, so it **ignores `idx_items_timestamp`**, reads every item, and then sorts:

```
== timeline for one day (current)
   SCAN items
   ...
   USE TEMP B-TREE FOR ORDER BY

== same query without GROUP BY
   SEARCH items USING INDEX idx_items_timestamp (timestamp>? AND timestamp<?)
```

The "find the most recent item" pre-search that the timeline page runs first (`controllers.js:276`, `limit: 1`) has the same problem: it reads and sorts **every item** to return one row.

**Fix:** stop joining `relationships`/`relations` into the main query. Express the conditions as subqueries so that no duplicate rows can occur, then drop the `GROUP BY`:

```sql
-- root items only
AND NOT EXISTS (
    SELECT 1 FROM relationships r JOIN relations rl ON rl.id = r.relation_id
    WHERE r.to_item_id = items.id AND rl.directed = 1 AND rl.subordinating = 1)
-- relation label filter (e.g. NOT motion)
AND NOT EXISTS (
    SELECT 1 FROM relationships r JOIN relations rl ON rl.id = r.relation_id
    WHERE r.to_item_id = items.id AND rl.label = 'motion')
```

The `entity_attributes` join can also multiply rows if an attribute belongs to more than one entity. Pick a single entity with a correlated subquery, or add `LIMIT 1` logic.

### 1.3 Timestamp `OR timespan` defeats half of the range

With a start date the filter is `(items.timestamp >= ? OR items.timespan >= ?) AND items.timestamp <= ?`. Because of the `OR`, SQLite can only use the upper bound and scans every item *before* the end date. Options:

* Add a partial index `CREATE INDEX ... ON items(timespan) WHERE timespan IS NOT NULL;` and use a `UNION` of the two ranges.
* Simpler: `timestamp BETWEEN ? AND ?` **or** `(timespan IS NOT NULL AND timestamp < ? AND timespan >= ?)`, with a bounded look-back (for example, the longest timespan stored).

### 1.4 N+1 queries when expanding relationships

`expandRelationships` → `expandRelationshipSingle` → `loadRelatedItem` issues 1 + (relationships × 2) queries per result item, recursively. For 2,000 messages that each have a sender, recipients and reactions, this easily reaches **10,000+ queries per page load**, all on one read transaction.

Fix: after the main query, collect all result IDs and fetch every relationship in **one** query (`WHERE from_item_id IN (...) OR to_item_id IN (...)`), then fetch the referenced items in a second batched query. Two or three queries instead of thousands.

`loadRelatedItem` also has a TODO about caching entities. An in-memory map for the duration of one request (attribute ID → entity) would remove most of the repeated entity lookups.

### 1.5 `extended_items` view computes unused columns

The view computes `day_of_year`, `week_of_year` and `month_of_year` with `strftime(date(...))` and joins `data_sources`/`classifications`. SQLite evaluates view columns lazily, so this is cheap if nothing references them. Still worth checking that `itemDBColumns` does not select them in the hot path; if it does, that is three `strftime` calls per row.

### 1.6 `extended_items` view causes a bad plan for very small limits

Found while measuring the fixes above. With `LIMIT 1` or `LIMIT 2`, which the timeline's "find the most recent item" pre-search uses, SQLite walks `items` in `(data_source_id, original_id)` order to suit the view's join to `data_sources`, and then sorts the whole table. The same query with `LIMIT 100`, or against `items` directly without the view, takes under 1 ms. Possible fixes:

* redefine `extended_items` to get `data_source_name`/`data_source_title`/`classification_name` through scalar subqueries instead of `LEFT JOIN`s (needs a `DROP VIEW` + `CREATE VIEW` migration, because `schema.sql` uses `IF NOT EXISTS`); or
* have the pre-search request a few more rows (for example `limit: 10`) and use the first.

### 1.7 SQLite connection settings

In `timeline/db.go`:

* **No `busy_timeout`.** Reads and writes during an import can fail with `database is locked` instead of waiting. Add `_busy_timeout=5000`.
* **Default page cache (about 2 MB).** On a server with RAM to spare, `PRAGMA cache_size=-65536` (64 MB) and `PRAGMA mmap_size=268435456` (keep mmap disabled on the exFAT/macOS path) can make reads several times faster on large timelines.
* `_synchronous=NORMAL` is the recommended setting with WAL. It makes imports much faster and is still crash-safe (you can lose only the last transaction on power loss, and the database is never corrupted).
* `PRAGMA temp_store=MEMORY` helps with the temp B-trees used for sorting.
* `ReadPool` has no `SetMaxOpenConns`/`SetMaxIdleConns`. Idle read connections get closed and reopened, and each new connection starts with a cold page cache. Set something like `SetMaxIdleConns(runtime.NumCPU())`.
* Run `ANALYZE` (or `PRAGMA optimize=0x10002`, which already runs) **after large imports**, so the planner has statistics. With no stats, the planner guesses badly on big tables.

---

## 2. Timeline page (frontend)

Files: `frontend/resources/js/timeline.js`, `controllers.js`, `common-lib.js`.

### 2.1 Everything is loaded and rendered at once

```js
// timeline.js
// offset: ..., // TODO: figure out how to paginate the timeline
limit: 2000
```

* 2,000 items, each with related items, become a very large uncompressed JSON response (often several MB).
* `timelineGroups()` and `renderTimelineGroups()` then build the whole DOM in a single synchronous pass: a template clone per item, Luxon `DateTime.fromISO` + `toLocaleString` per message, avatars, reaction tooltips and more. On an older CPU this blocks the main thread for seconds.
* `console.log("BATCH PARAMS, RESULTS:", ...)`, `console.log("GROUPS:", ...)` and `console.log("ALL GROUPS:", ...)` dump the entire result set to the console. With DevTools open, the browser keeps these objects alive and makes them inspectable, which costs memory and time. Remove them or put them behind a debug flag.

**Fix:** paginate by timestamp (keyset pagination: `start_timestamp = last item timestamp`, `limit: 150`) and append groups as an `IntersectionObserver` sentinel at the bottom comes into view. The old commented-out batching code in `timeline.js` is a starting point; it "didn't work well" mainly because it looped until 50 groups were collected instead of loading on scroll.

Other cheap wins:

* Add `content-visibility: auto; contain-intrinsic-size: auto 400px;` to `.list-timeline-item` (or each group card). The browser then skips layout and paint for off-screen groups, which is a big gain for long timelines and costs one CSS rule.
* Create one `Intl.DateTimeFormat` and reuse it instead of calling Luxon `toLocaleString` for every message.
* Only initialise Bootstrap tooltips on hover (event delegation) rather than per element.

### 2.2 Video thumbnails autoplay

In timeline and preview contexts, `itemContentElement` creates `<video autoplay loop muted>` for video thumbnails (WebM/VP8 from `libvpx`). Browsers usually have no hardware VP8 decoder, so **every visible video thumbnail is software-decoded continuously**. Ten videos in view can use a whole CPU core.

**Fix:** show a still poster image (generate a single-frame WebP/JPEG with ffmpeg `-vframes 1`) and play only on hover or tap. At minimum, pause videos that scroll out of view with an `IntersectionObserver`, and set `preload="none"`.

### 2.3 Mapbox GL without a GPU

`common-lib.js:332`:

```js
tlz.map = new mapboxgl.Map({
    container: document.createElement('div'),
    style: `mapbox://styles/mapbox/standard?optimized=true`,
    antialias: true
});
```

On top of that, the app adds a terrain DEM source, `updateMapLighting()` every 60 seconds, and `fitBounds`/`flyTo` animations whenever the map moves between placeholders.

* Without hardware acceleration, Chrome and Firefox fall back to SwiftShader (software WebGL). The Mapbox **Standard** style (3D buildings, landmarks, dynamic lighting, shadows) is the heaviest style Mapbox offers, and `antialias: true` adds MSAA on top. This is the most likely cause of the general sluggishness on a machine with no GPU.
* The map is created at app start-up on **every page**, not just pages with maps.

**Fixes:**

1. Add a setting such as "Low-power maps" (or detect software rendering by checking `WEBGL_debug_renderer_info` for "SwiftShader"/"llvmpipe") that:
   * uses `mapbox://styles/mapbox/streets-v12` or `light-v11` (2D) instead of `standard`
   * sets `antialias: false`, `fadeDuration: 0`, `projection: 'mercator'` (the globe projection costs more)
   * skips terrain and `updateMapLighting`
   * passes `duration: 0` to `fitBounds`/`flyTo`
2. For the small 16:9 maps in the timeline, use the **Mapbox Static Images API** (a plain `<img>`) and switch to the interactive map only on click. This removes nearly all WebGL work from the timeline.
3. Create the map lazily, the first time a map container appears.
4. Up to 100 `mapboxgl.Marker`s per location group are created as DOM elements. Use a GeoJSON `circle` layer instead; it is much cheaper.
5. The `document` `mousemove` listener calls `getBoundingClientRect()` on every map in view on **every mouse move**. Throttle it with `requestAnimationFrame`.

### 2.4 Global JS/CSS cost

`index.html` loads **every** vendored library and every page script on every page:

| File | Size (served uncompressed) |
|---|---|
| `mapbox-gl-js-v3.13.0.min.js` | 1.5 MB |
| `echarts-v6.0.0.min.js` | 1.1 MB (used on the dashboard only) |
| `imask.js` (not minified) | 100 KB |
| everything else | ~600 KB |

On an older CPU, parsing and compiling about 3.5 MB of JS on each full load is noticeable. Load ECharts (and possibly Mapbox) with a dynamic `import()` or a `<script>` injected only by the pages that need them, and use `imask.min.js`.

### 2.5 CSS `backdrop-filter`

`.translucent` (sticky navbar on the items page) uses `backdrop-filter: blur(15px)`, and the loader overlay uses `blur(10px)`. Without GPU compositing, a blurred sticky element forces a software re-blur on **every scroll frame**. Use a solid or semi-transparent background as a fallback, for example under `@media (prefers-reduced-motion)` or in a "low-power" body class.

---

## 3. Thumbnails, media and ML (server CPU)

### 3.1 AVIF thumbnails

`thumbnailType()` always returns `image/avif` for images (`thumbnails.go:1019`), and `serveThumbnail` defaults to AVIF.

* AV1 encoding is CPU-heavy even at `Effort: 1`.
* **AVIF decoding in the browser is also CPU-heavy** when there is no hardware AV1 decoder (most pre-2020 CPUs/GPUs). A gallery or timeline full of 720 px AVIF thumbnails costs much more client CPU than WebP or JPEG.

**Fix:** make the thumbnail format configurable (WebP at Q≈70 decodes several times faster than AVIF, and JPEG faster still). Also consider a smaller grid thumbnail (for example 360 px) separate from the 720 px image used for embeddings: `maxThumbnailDimension = 720` exists for ML quality, but the UI rarely needs that much.

### 3.2 On-demand thumbnail generation while browsing

`Timeline.Thumbnail()` generates a missing thumbnail **synchronously inside the HTTP request**, gated by `cpuIntensiveThrottle` (75% of cores). Scrolling a timeline whose thumbnail job has not finished queues dozens of AVIF encodes and ffmpeg runs, and the requests wait behind them. Check that the thumbnail job completed after import. Better still, have the HTTP path return the thumbhash or a placeholder immediately and queue the work, instead of blocking.

### 3.3 Thumbnail HTTP caching

`serveThumbnail` uses `http.ServeContent` with a modtime but **no `ETag` or `Cache-Control`**. Thumbnails are effectively immutable (keyed by data file/ID). Add:

```go
w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
w.Header().Set("ETag", ...) // data hash
```

so the browser never asks again. Each re-request currently means a SQLite blob read plus a thumbhash query.

### 3.4 Video thumbnails and transcoding

* Video thumbnails are 5-second VP8 WebMs at 256 kbit/s with `-threads 4` (fixed). With `cpuIntensiveThrottle` already running N jobs in parallel, `-threads 4` oversubscribes the CPU. Use `-threads 1` when several jobs run in parallel, or `-deadline realtime -cpu-used 8` for libvpx (it is much faster).
* `videos.go` transcodes with libvpx/libvorbis on Linux and has **no hardware-accelerated path** (VAAPI, QSV or NVENC) other than macOS VideoToolbox. Even an old Intel iGPU can usually do `h264_vaapi`/`h264_qsv`, which is far faster than software VP8. Worth making configurable.
* For a still thumbnail of a video (see 2.2), one frame with `-ss 1 -frames:v 1` is about 100× cheaper than an animated clip.

### 3.5 Python ML server (CPU only)

`tlzapp/python/server/server.py`:

* **The model is loaded twice:** `AutoModel.from_pretrained(...)` and then `pipeline(..., model=self.model_name)`, which loads its own copy. Pass `model=self.model` (and the processor) to `pipeline` to halve RAM use.
* `torch` comes from PyPI in `pyproject.toml`. On Linux this installs the **CUDA build (several GB)** even on machines with no NVIDIA GPU. Use the CPU wheel index for CPU-only installs:

  ```toml
  [tool.uv.sources]
  torch = { index = "pytorch-cpu" }
  torchvision = { index = "pytorch-cpu" }
  [[tool.uv.index]]
  name = "pytorch-cpu"
  url = "https://download.pytorch.org/whl/cpu"
  explicit = true
  ```

* `embeddingJob` sends **10 concurrent single-image requests** to the Flask server. On CPU, PyTorch already uses every core for each request, so concurrency oversubscribes threads and thrashes the cache. Sending one **batched** request of 8–32 images is much faster. Alternatively, call `torch.set_num_threads()` and use a single worker.
* Use `torch.inference_mode()` instead of `no_grad()` (slightly faster), and consider `torch.set_float32_matmul_precision("medium")` or bf16 on CPUs that support it.
* **Semantic search runs the classifier at query time** (`Search` → `classify()` over every image result). On CPU this can take tens of seconds per search. Cache the results, cap how many results are classified, or rely on embedding distance alone in low-power mode.
* If the server has no GPU, **turn off "semantic features"** in settings (`semantic_features` property). This skips the embedding job after every import, which is the most CPU-intensive background task.

---

## 4. HTTP server

* **No response compression.** Neither JSON API responses (search results can be MB) nor static JS/CSS are gzip/zstd-compressed. The app already depends on Caddy, so `caddyhttp/encode` or a small `gzip` middleware works; alternatively, pre-compress the embedded vendor files at build time.
* **Static assets have no caching validators.** They come from `embed.FS` via `http.FileServer`, and embedded files have a zero modtime, so no `Last-Modified` header is sent and there is no `ETag`. The browser has to re-download about 3.5 MB of JS on every full reload. The vendor files already have versions in their filenames, so serve `/resources/js/vendor/*` and `/resources/css/vendor/*` with `Cache-Control: max-age=31536000, immutable`, and add a build-hash query string for the app's own files.
* **Every request is logged at INFO** (`server.go:ServeHTTP`), including each of the hundreds of thumbnail requests a timeline view makes. Writing and encoding those logs (and pushing them over the log websocket to the UI) costs CPU. Log successful static and thumbnail requests at DEBUG.

---

## 5. Maintenance: "hasn't been updated in a while"

The last upstream commit in this fork is **21 May 2026** (`go.mod: Upgrade dependencies`), so the Go dependencies were refreshed recently. Things that are behind or fragile:

| Item | Current | Notes |
|---|---|---|
| Mapbox GL JS | 3.13.0 (vendored) | Several newer 3.x releases bring rendering performance work. Updating the vendored file is a drop-in change. |
| Luxon | 3.5.0 | Newer 3.x available; drop-in. |
| noUiSlider | 15.6.1 | Minor updates available. |
| Tabler | vendored `tabler.min.*` (version not in filename) | Record the version; update to the latest 1.x. |
| `imask.js` | Unminified | Swap for the minified build. |
| Two date pickers | `air-datepicker` **and** `easepick` | Both are loaded; check whether `easepick` is still used and drop it if not. |
| Dockerfile: libvips | `git clone --depth 1` of libvips **HEAD** | Unpinned: every Docker build gets whatever is on libvips `master`, which can break builds or behaviour. Pin to a release tag (for example `v8.17.x`). |
| Dockerfile: ffmpeg | Debian trixie-backports | Fine; consider `intel-media-va-driver`/`mesa-va-drivers` + `vainfo` if adding VAAPI (3.4). |
| CI | `go-version: stable` | Fine for tracking, but makes builds non-reproducible; consider matching `go.mod` (`go 1.25.8`). |
| Python | `requires-python = "~=3.13.0"` | Fine; the lock file pins versions. Regenerate `uv.lock` periodically for security fixes in `transformers`/`flask`/`pillow`. |
| Default Mapbox token | Hard-coded in `common-lib.js` | Shared token; if it gets rate-limited, maps break for everyone. Make it obvious in settings that users should set their own. |

Code hygiene items noticed along the way:

* `timeline.js` and `controllers.js` contain about 200 lines of commented-out legacy code, which makes the real code harder to find.
* `tl_clear()` in `common-lib.js` loops over `tl_data.sources` but calls `tl_removeLayer(sourceID)` instead of `tl_removeSource(sourceID)`. Sources are never removed, so they pile up on the map across moves (memory, plus work for the map on every re-render).
* `Search()` opens a read transaction and never commits it (`defer tx.Rollback()`). That works, but on a busy WAL database the transaction holds a read snapshot for the whole duration of the N+1 expansion (1.4), which blocks WAL checkpointing during imports. Fixing 1.4 makes the transaction short.

---

## 6. Things you can do right now, without code changes

1. **Make sure hardware acceleration is on in the browser you view from.** Visit `chrome://gpu` (or `about:support` in Firefox). If WebGL shows "Software only" or "SwiftShader", maps will always be slow. If you view the app from another machine with a GPU (your laptop, say), you avoid the server's lack of a GPU entirely: rendering happens in the **browser**, not on the server.
2. **Turn off semantic features** (Settings) if the server has no GPU. This stops embedding jobs.
3. **Let the thumbnail job finish** after an import before browsing, so thumbnails don't get generated while you browse.
4. **Narrow the timeline filter** (one day, one data source) to cut the 2,000-item load.
5. **Keep the timeline on fast local storage** (SSD; not exFAT, network shares or USB HDD). SQLite and thumbnail reads are latency-bound.
6. Run a one-off `sqlite3 timeline.db "ANALYZE;"` (with the app stopped) after big imports.
7. **Add the missing index yourself** (app stopped, take a backup first):

   ```sh
   sqlite3 /path/to/timeline/timeline.db \
     "CREATE INDEX IF NOT EXISTS idx_relationships_to_item_id ON relationships(to_item_id); ANALYZE;"
   ```

   This alone should make a noticeable difference to timeline, conversation and item page load times.

---

## Suggested order of work

1. Add the `relationships(to_item_id)` index (one line in `schema.sql`).
2. Remove `GROUP BY items.id` by using `EXISTS` subqueries; batch the relationship expansion.
3. Timeline pagination and infinite scroll, plus `content-visibility: auto`.
4. A "low-power" map mode and static map images in the timeline.
5. Gzip middleware and cache headers for static files and thumbnails.
6. A configurable thumbnail format (WebP), still posters for videos, no autoplay.
7. ML server: single model load, CPU torch wheel, batched embedding requests.
8. Pin libvips in the Dockerfile; bump the vendored front-end libraries.
