package forgejo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestListRepoCommentsPaginates(t *testing.T) {
	since := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token sekrit" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/git/api/v1/repos/ops/infra/issues/comments" || r.URL.Query().Get("since") != "2026-10-29T10:00:00Z" {
			http.Error(w, "bad request "+r.URL.String(), http.StatusBadRequest)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		pages = append(pages, r.URL.Query().Get("page"))
		n := pageSize
		if page == 2 {
			n = 3
		}
		out := make([]Comment, n)
		for i := range out {
			out[i] = Comment{ID: int64(page*1000 + i)}
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	c, _ := New(srv.URL+"/git/", "sekrit", "peon/test")
	got, err := c.ListRepoComments(context.Background(), "ops/infra", since)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != pageSize+3 || fmt.Sprint(pages) != "[1 2]" {
		t.Fatalf("got %d comments over pages %v", len(got), pages)
	}
}

func TestCreateCommentAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/repos/ops/infra/issues/42/comments":
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			json.NewEncoder(w).Encode(Comment{ID: 5, Body: in["body"], User: User{ID: 31, Login: "peon"}})
		case r.URL.Path == "/api/v1/repos/ops/infra/issues/comments/404":
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
		}
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "t", "ua")
	ctx := context.Background()

	got, err := c.CreateComment(ctx, "ops/infra", 42, "hello\n```x```")
	if err != nil || got.ID != 5 || got.Body != "hello\n```x```" {
		t.Fatalf("got %+v, err %v", got, err)
	}
	_, err = c.GetComment(ctx, "ops/infra", 404)
	if !IsNotFound(err) {
		t.Fatalf("error = %v, want not found", err)
	}
	_, err = c.ListCommentReactions(ctx, "ops/infra", 1)
	if err == nil || IsNotFound(err) {
		t.Fatalf("error = %v, want HTTP 418", err)
	}
}

func TestIssueIndex(t *testing.T) {
	cases := map[Comment]int64{
		{IssueURL: "https://f/ops/infra/issues/42"}:      42,
		{PRURL: "https://f/ops/infra/pulls/7"}:           7,
		{IssueURL: "https://f/api/v1/repos/x/issues/9/"}: 9,
	}
	for c, want := range cases {
		if got, err := c.IssueIndex(); err != nil || got != want {
			t.Errorf("IssueIndex(%+v) = %d, %v", c, got, err)
		}
	}
	for _, c := range []Comment{{}, {IssueURL: "https://f/ops/infra/issues/abc"}} {
		if _, err := c.IssueIndex(); err == nil {
			t.Errorf("IssueIndex(%+v) succeeded", c)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":                              0,
		"120":                           2 * time.Minute,
		"-5":                            0,
		"soon":                          0,
		"Thu, 29 Oct 2026 10:05:00 GMT": 5 * time.Minute,
		"Thu, 29 Oct 2026 09:00:00 GMT": 0,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestRetryAfterAndTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/o/r/issues/comments/1":
			w.Header().Set("Retry-After", "30")
			http.Error(w, "slow down", http.StatusTooManyRequests)
		case "/api/v1/repos/o/r/issues/comments/2":
			http.Error(w, "gone", http.StatusNotFound)
		default:
			http.Error(w, "boom", http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	c, _ := New(srv.URL, "t", "ua")
	ctx := context.Background()

	_, err := c.GetComment(ctx, "o/r", 1)
	var he *HTTPError
	if !errors.As(err, &he) || he.RetryAfter != 30*time.Second || !Transient(err) {
		t.Fatalf("429: %v", err)
	}
	if _, err := c.GetComment(ctx, "o/r", 2); Transient(err) {
		t.Fatalf("404 treated as an outage: %v", err)
	}
	if _, err := c.GetComment(ctx, "o/r", 3); !Transient(err) {
		t.Fatalf("502 not treated as an outage: %v", err)
	}
	if !Transient(errors.New("dial tcp: connection refused")) || Transient(nil) {
		t.Fatal("transport errors")
	}
}
