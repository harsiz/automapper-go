package automapper

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func valid() Settings {
	return Settings{Format: FormatOsz, Title: "Blue Zenith", Artist: "xi", StarRating: 6.5, Year: 2020}
}

func newTest(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New("amk_test", WithBaseURL(srv.URL), WithPollInterval(time.Millisecond))
}

func TestValidate(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
	cases := map[string]func(*Settings){
		"format":           func(s *Settings) { s.Format = "zip" },
		"title":            func(s *Settings) { s.Title = "" },
		"artist":           func(s *Settings) { s.Artist = strings.Repeat("a", 121) },
		"star_rating":      func(s *Settings) { s.StarRating = 1 },
		"year":             func(s *Settings) { s.Year = 2024 },
		"ar":               func(s *Settings) { s.AR = Float(11) },
		"slider_tick_rate": func(s *Settings) { s.SliderTickRate = 5 },
		"go_astray":        func(s *Settings) { s.Descriptors = []string{"jumps/wide"} },
		"game":             func(s *Settings) { s.Game = "taiko" },
	}
	for field, mutate := range cases {
		s := valid()
		mutate(&s)
		var ve *ValidationError
		if err := s.Validate(); !errors.As(err, &ve) || ve.Fields[field] == "" {
			t.Errorf("%s: want field error, got %v", field, err)
		}
	}
}

func TestCreateSendsMultipartSettingsThenAudio(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/maps" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer amk_test" {
			t.Errorf("auth header = %q", got)
		}
		mr, err := r.MultipartReader()
		if err != nil {
			t.Fatal(err)
		}
		p1, _ := mr.NextPart()
		b1, _ := io.ReadAll(p1)
		p2, _ := mr.NextPart()
		b2, _ := io.ReadAll(p2)
		if p1.FormName() != "settings" || !strings.Contains(string(b1), `"title":"Blue Zenith"`) {
			t.Errorf("part 1 = %s %s", p1.FormName(), b1)
		}
		if strings.Contains(string(b1), `"hp"`) {
			t.Errorf("unset optional hp was sent: %s", b1)
		}
		if p2.FormName() != "audio" || p2.FileName() != "song.mp3" || string(b2) != "AUDIO" {
			t.Errorf("part 2 = %s %s %q", p2.FormName(), p2.FileName(), b2)
		}
		w.WriteHeader(202)
		io.WriteString(w, `{"id":"abc","status":"queued","quota":{"limit":50,"used":0,"in_flight":1,"remaining":50,"resets_at":"2026-10-11T00:00:00Z"}}`)
	})
	job, err := c.Create(context.Background(), strings.NewReader("AUDIO"), "dir/song.mp3", valid())
	if err != nil {
		t.Fatal(err)
	}
	if job.ID != "abc" || job.Quota.Remaining != 50 || job.Quota.ResetsAt.Year() != 2026 {
		t.Errorf("job = %+v", job)
	}
}

func TestCreateValidatesBeforeNetwork(t *testing.T) {
	c := newTest(t, func(http.ResponseWriter, *http.Request) { t.Error("request should not be sent") })
	s := valid()
	s.Year = 1999
	if _, err := c.Create(context.Background(), strings.NewReader("x"), "a.mp3", s); err == nil {
		t.Fatal("expected error")
	}
}

func TestAPIErrors(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/maps/quota":
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(429)
			io.WriteString(w, `{"error":"used up","code":"quota_exceeded"}`)
		case "/maps/bad":
			w.WriteHeader(422)
			io.WriteString(w, `{"error":"bad","fields":{"star_rating":"too high"}}`)
		case "/maps/nokey":
			w.WriteHeader(401)
			io.WriteString(w, `{"error":"no key","code":"unauthorized"}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, "not json")
		}
	})
	ctx := context.Background()

	_, err := c.Get(ctx, "quota")
	var ae *APIError
	if !errors.As(err, &ae) || ae.RetryAfter != 120*time.Second || !IsQuotaExceeded(err) || !IsRateLimit(err) {
		t.Errorf("quota error = %#v", err)
	}
	_, err = c.Get(ctx, "bad")
	if !errors.As(err, &ae) || ae.Fields["star_rating"] != "too high" {
		t.Errorf("fields error = %#v", err)
	}
	if _, err = c.Get(ctx, "nokey"); !IsUnauthorized(err) {
		t.Errorf("want unauthorized, got %v", err)
	}
	if _, err = c.Get(ctx, "nope"); !IsNotFound(err) {
		t.Errorf("want not found, got %v", err)
	}
}

func TestWaitPollsUntilDone(t *testing.T) {
	var n int32
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		st := "queued"
		switch atomic.AddInt32(&n, 1) {
		case 2:
			st = "running"
		case 3:
			st = "done"
		}
		io.WriteString(w, `{"id":"abc","status":"`+st+`","position":3,"queued":4}`)
	})
	m, err := c.Wait(context.Background(), "abc")
	if err != nil || !m.Done() || n != 3 {
		t.Fatalf("m=%+v err=%v polls=%d", m, err, n)
	}
}

func TestWaitFailedAndCancel(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"abc","status":"failed","message":"no clear beat"}`)
	})
	_, err := c.Wait(context.Background(), "abc")
	var fe *FailedError
	if !errors.As(err, &fe) || fe.Map.Message != "no clear beat" {
		t.Errorf("err = %v", err)
	}

	c = newTest(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"status":"running"}`) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Wait(ctx, "abc"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want deadline exceeded, got %v", err)
	}
}

func TestGenerateAndSave(t *testing.T) {
	var srvURL string
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST":
			w.WriteHeader(202)
			io.WriteString(w, `{"id":"abc","status":"queued"}`)
		case r.URL.Path == "/maps/abc":
			io.WriteString(w, `{"id":"abc","status":"done","download_url":"`+srvURL+`/dl/x.osz"}`)
		case r.URL.Path == "/dl/x.osz":
			io.WriteString(w, "OSZDATA")
		}
	})
	srvURL = c.baseURL
	dir := t.TempDir()
	audio := filepath.Join(dir, "song.mp3")
	os.WriteFile(audio, []byte("AUDIO"), 0o644)
	out := filepath.Join(dir, "out.osz")

	if _, err := c.GenerateAndSave(context.Background(), audio, out, valid()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(out); string(b) != "OSZDATA" {
		t.Errorf("saved %q", b)
	}
}

func TestSearchQuery(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("q") != "blue zenith" || q.Get("page") != "2" || q.Get("per_page") != "10" {
			t.Errorf("query = %v", q)
		}
		io.WriteString(w, `{"maps":[{"id":"a"},{"id":"b"}],"page":2}`)
	})
	l, err := c.Search(context.Background(), SearchOptions{Query: "blue zenith", Page: 2, PerPage: 10})
	if err != nil || len(l.Maps) != 2 {
		t.Fatalf("l=%+v err=%v", l, err)
	}
}

func TestMe(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"username":"harsiz","quota":{"limit":50,"used":12,"remaining":37}}`)
	})
	me, err := c.Me(context.Background())
	if err != nil || me.Quota.Remaining != 37 || me.Raw["username"] != "harsiz" {
		t.Fatalf("me=%+v err=%v", me, err)
	}
}

func TestGenerateOnUpdateEvents(t *testing.T) {
	var polls int32
	var srvURL string
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST":
			w.WriteHeader(202)
			io.WriteString(w, `{"id":"abc","status":"queued","map_url":"https://x/map/abc"}`)
		default:
			switch atomic.AddInt32(&polls, 1) {
			case 1:
				io.WriteString(w, `{"id":"abc","status":"queued","position":2}`)
			case 2:
				io.WriteString(w, `{"id":"abc","status":"queued","position":1}`)
			case 3:
				io.WriteString(w, `{"id":"abc","status":"running"}`)
			case 4:
				io.WriteString(w, `{"id":"abc","status":"running"}`)
			default:
				io.WriteString(w, `{"id":"abc","status":"done","map_url":"https://x/map/abc","download_url":"`+srvURL+`/dl.osz"}`)
			}
		}
	})
	srvURL = c.baseURL
	audio := filepath.Join(t.TempDir(), "s.mp3")
	os.WriteFile(audio, []byte("A"), 0o644)

	var events []string
	var last Update
	m, err := c.Generate(context.Background(), audio, valid(), OnUpdate(func(u Update) {
		events = append(events, u.Event)
		last = u
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := "submitted queued queued started done"
	if got := strings.Join(events, " "); got != want {
		t.Errorf("events = %q, want %q", got, want)
	}
	if last.Map.DownloadURL == "" || last.Map.MapURL != "https://x/map/abc" || m.DownloadURL != last.Map.DownloadURL {
		t.Errorf("final update = %+v", last.Map)
	}
}
