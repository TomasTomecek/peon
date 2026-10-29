package loki

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 29, 10, 0, 0, 5, time.UTC)

func TestBodyGroupsStreams(t *testing.T) {
	a := map[string]string{"job": "peon", "event": "stdout"}
	b := map[string]string{"event": "stderr", "job": "peon"}
	body, err := Body([]Entry{{a, t0, "one"}, {b, t0, "err"}, {a, t0.Add(1), "two"}})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Streams []struct {
			Stream map[string]string `json:"stream"`
			Values [][2]string       `json:"values"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Streams) != 2 || len(got.Streams[0].Values) != 2 || got.Streams[0].Values[1][1] != "two" {
		t.Fatalf("unexpected payload %s", body)
	}
	if got.Streams[0].Values[0][0] != "1793268000000000005" {
		t.Fatalf("timestamp = %s", got.Streams[0].Values[0][0])
	}
}

func TestClientPush(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		gotAuth, gotPath = u+":"+p, r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	c := New(srv.URL+"/", "peon-db1", "pw", "peon/test")
	if err := c.Push(context.Background(), []Entry{{map[string]string{"job": "peon"}, t0, "hi"}}); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "peon-db1:pw" || gotPath != "/loki/api/v1/push" || !strings.Contains(string(gotBody), `"hi"`) {
		t.Fatalf("auth=%q path=%q body=%s", gotAuth, gotPath, gotBody)
	}

	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "entry out of order", http.StatusBadRequest)
	}))
	defer fail.Close()
	err := New(fail.URL, "u", "p", "ua").Push(context.Background(), []Entry{{map[string]string{"job": "peon"}, t0, "x"}})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("error = %v", err)
	}
}

type fakePusher struct {
	fail  bool
	got   []Entry
	calls int
}

func (f *fakePusher) Push(_ context.Context, e []Entry) error {
	f.calls++
	if f.fail {
		return errors.New("down")
	}
	f.got = append(f.got, e...)
	return nil
}

func TestStreamSpoolsAndKeepsOrder(t *testing.T) {
	sp, err := NewSpool(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := &fakePusher{fail: true}
	s := NewStream(p, sp, map[string]string{"job": "peon"})
	ctx := context.Background()

	s.Add(t0, "1")
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	p.fail = false
	// Loki is back, but line 1 is still spooled: line 2 must queue behind it.
	s.Add(t0, "2")
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(p.got) != 0 || sp.Pending() != 2 {
		t.Fatalf("pushed %d entries, %d spooled", len(p.got), sp.Pending())
	}
	n, err := sp.Flush(ctx, p)
	if err != nil || n != 2 {
		t.Fatalf("flushed %d, err %v", n, err)
	}
	if len(p.got) != 2 || p.got[0].Line != "1" || p.got[1].Line != "2" || sp.Pending() != 0 {
		t.Fatalf("order or count wrong: %+v", p.got)
	}
}

func TestSpoolFlushStopsAtFirstError(t *testing.T) {
	sp, _ := NewSpool(t.TempDir())
	_ = sp.Add([]Entry{{nil, t0, "a"}})
	_ = sp.Add([]Entry{{nil, t0, "b"}})
	p := &fakePusher{fail: true}
	if n, err := sp.Flush(context.Background(), p); err == nil || n != 0 || p.calls != 1 {
		t.Fatalf("n=%d err=%v calls=%d", n, err, p.calls)
	}
	if sp.Pending() != 2 {
		t.Fatalf("pending = %d", sp.Pending())
	}
}

func TestStreamTruncatesHugeLines(t *testing.T) {
	p := &fakePusher{}
	s := NewStream(p, nil, nil)
	s.Add(t0, strings.Repeat("x", MaxLine+10))
	_ = s.Flush(context.Background())
	if l := p.got[0].Line; len(l) > MaxLine+64 || !strings.HasSuffix(l, "[peon: line truncated]") {
		t.Fatalf("line length %d", len(l))
	}
}
