package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/storage"
)

const adminTestKey = "an-admin-key-for-tests-0123456789"

// adminHdr authenticates against the admin API with the app key.
var adminHdr = map[string]string{"Authorization": "Bearer " + adminTestKey}

func newAdminEnv(t *testing.T, pools map[string][]string) *testEnv {
	t.Helper()
	return newTestEnv(t, "[app]\napp_key = \""+adminTestKey+"\"\n", pools)
}

func (e *testEnv) record(tok string) *account.Record {
	e.t.Helper()
	recs, err := e.repo.GetAccounts(context.Background(), []string{tok})
	if err != nil || len(recs) == 0 || recs[0] == nil {
		e.t.Fatalf("account %s missing: %v", tok, err)
	}
	return recs[0]
}

// B7: editing a token without naming a pool must not demote it to "basic" nor
// drop its quota / usage state.
func TestTokensEditOmittedPoolKeepsPoolAndState(t *testing.T) {
	e := newAdminEnv(t, map[string][]string{"super": {tokA}})
	use := 7
	expert := map[string]any{"remaining": 3, "total": 50, "window_seconds": 7200}
	if _, err := e.repo.PatchAccounts(context.Background(), []account.Patch{{Token: tokA, UsageUseDelta: &use, QuotaExpert: &expert}}); err != nil {
		t.Fatal(err)
	}

	w := e.do(http.MethodPut, "/admin/api/tokens/edit", `{"old_token":"`+tokA+`","token":"`+tokB+`"}`, adminHdr)
	if w.Code != http.StatusOK {
		t.Fatalf("edit = %d: %s", w.Code, w.Body.String())
	}
	if got := jsonBody(t, w)["pool"]; got != "super" {
		t.Fatalf("response pool = %v, want super", got)
	}
	rec := e.record(tokB)
	if rec.Pool != "super" {
		t.Fatalf("pool = %q, want super", rec.Pool)
	}
	if rec.UsageUseCount != 7 {
		t.Fatalf("usage count = %d, want 7", rec.UsageUseCount)
	}
	if qs := rec.QuotaSet(); qs.Expert.Remaining != 3 {
		t.Fatalf("expert remaining = %d, want 3 (state dropped)", qs.Expert.Remaining)
	}
	if !e.record(tokA).IsDeleted() {
		t.Fatal("old token still active")
	}
}

func TestTokensEditExplicitPoolMovesAccount(t *testing.T) {
	e := newAdminEnv(t, map[string][]string{"basic": {tokA}})
	w := e.do(http.MethodPut, "/admin/api/tokens/edit", `{"old_token":"`+tokA+`","token":"`+tokA+`","pool":"heavy"}`, adminHdr)
	if w.Code != http.StatusOK {
		t.Fatalf("edit = %d: %s", w.Code, w.Body.String())
	}
	if rec := e.record(tokA); rec.Pool != "heavy" {
		t.Fatalf("pool = %q, want heavy", rec.Pool)
	}
	w = e.do(http.MethodPut, "/admin/api/tokens/edit", `{"old_token":"`+tokA+`","token":"`+tokA+`","pool":"nope"}`, adminHdr)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid pool = %d, want 400", w.Code)
	}
}

// B10: the repository serves at most maxAccountPageSize rows per page; the
// token list must paginate instead of silently truncating.
func TestTokensListPaginatesPastRepositoryCap(t *testing.T) {
	e := newAdminEnv(t, nil)
	n := maxAccountPageSize + 137
	ups := make([]account.Upsert, 0, n)
	for i := 0; i < n; i++ {
		ups = append(ups, account.Upsert{Token: fmt.Sprintf("tok-%06d-%s", i, "xxxxxxxxxxxxxxxx"), Pool: "basic"})
	}
	if _, err := e.repo.UpsertAccounts(context.Background(), ups); err != nil {
		t.Fatal(err)
	}
	w := e.do(http.MethodGet, "/admin/api/tokens", "", adminHdr)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d", w.Code)
	}
	toks := jsonBody(t, w)["tokens"].([]any)
	if len(toks) != n {
		t.Fatalf("listed %d tokens, want %d", len(toks), n)
	}
	seen := map[string]bool{}
	for _, it := range toks {
		seen[it.(map[string]any)["token"].(string)] = true
	}
	if len(seen) != n {
		t.Fatalf("duplicate tokens in listing: %d unique of %d", len(seen), n)
	}
}

func TestDeleteInvalidSeesAccountsPastRepositoryCap(t *testing.T) {
	e := newAdminEnv(t, nil)
	n := maxAccountPageSize + 5
	ups := make([]account.Upsert, 0, n)
	for i := 0; i < n; i++ {
		ups = append(ups, account.Upsert{Token: fmt.Sprintf("tok-%06d-%s", i, "xxxxxxxxxxxxxxxx"), Pool: "basic"})
	}
	if _, err := e.repo.UpsertAccounts(context.Background(), ups); err != nil {
		t.Fatal(err)
	}
	expired := account.StatusExpired
	var patches []account.Patch
	for i := n - 3; i < n; i++ { // the last ones sort past the first page
		patches = append(patches, account.Patch{Token: fmt.Sprintf("tok-%06d-%s", i, "xxxxxxxxxxxxxxxx"), Status: &expired})
	}
	if _, err := e.repo.PatchAccounts(context.Background(), patches); err != nil {
		t.Fatal(err)
	}
	w := e.do(http.MethodDelete, "/admin/api/tokens/invalid", "", adminHdr)
	if w.Code != http.StatusOK {
		t.Fatalf("delete invalid = %d: %s", w.Code, w.Body.String())
	}
	if got := jsonBody(t, w)["deleted"]; got != float64(3) {
		t.Fatalf("deleted = %v, want 3", got)
	}
}

// B16: a server without a local media cache must answer 503, not panic.
func TestCacheEndpointsWithoutMediaStore(t *testing.T) {
	e := newAdminEnv(t, nil)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/admin/api/cache/clear", `{"type":"video"}`},
		{http.MethodPost, "/admin/api/cache/item/delete", `{"name":"x.mp4","type":"video"}`},
		{http.MethodPost, "/admin/api/cache/items/delete", `{"names":["x.mp4"],"type":"video"}`},
	} {
		w := e.do(c.method, c.path, c.body, adminHdr)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s = %d, want 503: %s", c.path, w.Code, w.Body.String())
		}
	}
}

func TestCacheClearRejectsUnknownType(t *testing.T) {
	e := newAdminEnv(t, nil)
	w := e.do(http.MethodPost, "/admin/api/cache/clear", `{"type":"audio"}`, adminHdr)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if w := e.do(http.MethodPost, "/admin/api/cache/clear", `{not json`, adminHdr); w.Code != http.StatusBadRequest {
		t.Fatalf("malformed body = %d, want 400", w.Code)
	}
}

// B15: clearing upstream assets needs no refresh service and validates input.
func TestBatchCacheClearValidatesAndDoesNotNeedRefresh(t *testing.T) {
	e := newAdminEnv(t, nil)
	e.srv.Refresh = nil
	e.up.listAssets = func(token string) (map[string]any, error) {
		return map[string]any{"assets": []any{map[string]any{"id": "a1"}, map[string]any{"assetId": "a2"}}}, nil
	}
	deleted := 0
	e.up.deleteAsset = func(token, id string) (map[string]any, error) { deleted++; return nil, nil }

	if w := e.post("/admin/api/batch/cache-clear", `{"tokens":[]}`, adminHdr); w.Code != http.StatusBadRequest {
		t.Fatalf("empty tokens = %d, want 400", w.Code)
	}
	w := e.post("/admin/api/batch/cache-clear", `{"tokens":["`+tokA+`"]}`, adminHdr)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if deleted != 2 {
		t.Fatalf("deleted %d assets, want 2", deleted)
	}
}

func TestMediaTypeFromName(t *testing.T) {
	cases := map[string]struct {
		want storage.MediaType
		ok   bool
	}{
		"": {storage.MediaImage, true}, "image": {storage.MediaImage, true}, " Video ": {storage.MediaVideo, true},
		"video": {storage.MediaVideo, true}, "audio": {"", false},
	}
	for in, want := range cases {
		got, ok := mediaTypeFromName(in)
		if got != want.want || ok != want.ok {
			t.Fatalf("mediaTypeFromName(%q) = %q,%v", in, got, ok)
		}
	}
}

func TestSerializeRecordGolden(t *testing.T) {
	rec := &account.Record{
		Token:  "tok-serialize-0123456789",
		Status: account.StatusActive,
		Tags:   []string{},
		Quota:  account.DefaultQuotaSet("basic").ToMap(),
		Ext:    map[string]any{},
	}
	got := mustJSON(t, serializeRecord(rec))
	want := `{"last_used_at":0,"pool":"basic","quota":{"console":{"remaining":20,"total":20,"window_seconds":3600},"fast":{"remaining":30,"total":30,"window_seconds":86400}},"status":"active","tags":[],"token":"tok-serialize-0123456789","use_count":0}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestBuildTogglePatch(t *testing.T) {
	off := buildTogglePatch("t", true)
	if off.Status == nil || *off.Status != account.StatusDisabled || off.ExtMerge["disabled_reason"] != "operator_disabled" || off.ClearFailures {
		t.Fatalf("disable patch = %+v", off)
	}
	on := buildTogglePatch("t", false)
	if on.Status == nil || *on.Status != account.StatusActive || !on.ClearFailures {
		t.Fatalf("enable patch = %+v", on)
	}
}

func TestExtractAssetRowsAndItems(t *testing.T) {
	resp := map[string]any{"items": []any{
		map[string]any{"assetId": "a1", "fileName": "x.png", "fileSize": float64(10), "contentType": "image/png", "createdAt": "t0"},
		"junk",
		map[string]any{"id": "a2", "name": "y", "size": float64(3), "file_path": "p"},
	}}
	if got := extractAssetItems(resp); fmt.Sprint(got) != "[a1 a2]" {
		t.Fatalf("items = %v", got)
	}
	row := extractAssetRows("tok-0123456789abcdef", resp, nil)
	if row["count"] != 2 || row["masked"] != "tok-...cdef" {
		t.Fatalf("row = %v", row)
	}
	errRow := extractAssetRows("tok-0123456789abcdef", nil, fmt.Errorf("boom"))
	if errRow["error"] != "boom" || errRow["count"] != 0 {
		t.Fatalf("error row = %v", errRow)
	}
}
