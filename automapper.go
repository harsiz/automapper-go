// Package automapper is a tiny client for the osu! AutoMapper API
// (https://am.justharsiz.lol).
//
// The easiest way to make a map:
//
//	c := automapper.New(os.Getenv("AUTOMAPPER_KEY"))
//	m, err := c.GenerateAndSave(ctx, "song.mp3", "blue-zenith.osz", automapper.Settings{
//		Format: automapper.FormatOsz, Title: "Blue Zenith", Artist: "xi",
//		StarRating: 6.5, Year: 2020,
//	})
package automapper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// DefaultBaseURL is the production API root.
const DefaultBaseURL = "https://am.justharsiz.lol/api/v1"

// Output formats.
const (
	FormatOsz = "osz" // the map with audio
	FormatOsu = "osu" // the difficulty file only
)

// Map statuses.
const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// Client talks to the AutoMapper API. It is safe for concurrent use.
type Client struct {
	key          string
	baseURL      string
	http         *http.Client
	pollInterval time.Duration
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL overrides the API root (useful for tests).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = u } }

// WithHTTPClient swaps the underlying HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithPollInterval sets how often Wait polls (default 5s).
func WithPollInterval(d time.Duration) Option { return func(c *Client) { c.pollInterval = d } }

// New creates a Client from an API key (amk_...).
func New(apiKey string, opts ...Option) *Client {
	c := &Client{
		key:          apiKey,
		baseURL:      DefaultBaseURL,
		http:         &http.Client{Timeout: 5 * time.Minute},
		pollInterval: 5 * time.Second,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Settings describe the map to generate. Format, Title, Artist, StarRating
// and Year are required.
type Settings struct {
	Format          string   `json:"format"`
	Title           string   `json:"title"`
	Artist          string   `json:"artist"`
	ArtistRomanised string   `json:"artist_romanised,omitempty"`
	Mapper          string   `json:"mapper,omitempty"`
	Tags            string   `json:"tags,omitempty"` // space separated
	StarRating      float64  `json:"star_rating"`    // 2..12
	Year            int      `json:"year"`           // 2007..2023
	MapperID        int      `json:"mapper_id,omitempty"`
	HP              *float64 `json:"hp,omitempty"` // 0..10, nil lets the model pick
	AR              *float64 `json:"ar,omitempty"`
	CS              *float64 `json:"cs,omitempty"`
	OD              *float64 `json:"od,omitempty"`
	SliderTickRate  float64  `json:"slider_tick_rate,omitempty"` // 0.5, 1, 2, 3, 4
	GoAstray        bool     `json:"go_astray,omitempty"`
	CfgScale        *float64 `json:"cfg_scale,omitempty"` // 1..3, needs GoAstray
	Descriptors     []string `json:"descriptors,omitempty"`
	Background      *bool    `json:"background,omitempty"` // default true (osz only)
	Game            string   `json:"game,omitempty"`       // only "osu" for now
}

// Float returns a pointer to v, for the optional HP/AR/CS/OD/CfgScale fields.
func Float(v float64) *float64 { return &v }

// Bool returns a pointer to v, for Settings.Background.
func Bool(v bool) *bool { return &v }

// Validate checks the settings locally so obvious mistakes fail before upload.
func (s Settings) Validate() error {
	f := map[string]string{}
	if s.Format != FormatOsz && s.Format != FormatOsu {
		f["format"] = `must be "osz" or "osu"`
	}
	if s.Title == "" || len([]rune(s.Title)) > 120 {
		f["title"] = "required, up to 120 characters"
	}
	if s.Artist == "" || len([]rune(s.Artist)) > 120 {
		f["artist"] = "required, up to 120 characters"
	}
	if s.StarRating < 2 || s.StarRating > 12 {
		f["star_rating"] = "must be between 2 and 12"
	}
	if s.Year < 2007 || s.Year > 2023 {
		f["year"] = "must be between 2007 and 2023"
	}
	for name, p := range map[string]*float64{"hp": s.HP, "ar": s.AR, "cs": s.CS, "od": s.OD} {
		if p != nil && (*p < 0 || *p > 10) {
			f[name] = "must be between 0 and 10"
		}
	}
	if s.CfgScale != nil && (*s.CfgScale < 1 || *s.CfgScale > 3) {
		f["cfg_scale"] = "must be between 1 and 3"
	}
	if (s.CfgScale != nil || len(s.Descriptors) > 0) && !s.GoAstray {
		f["go_astray"] = "must be true to use cfg_scale or descriptors"
	}
	switch s.SliderTickRate {
	case 0, 0.5, 1, 2, 3, 4:
	default:
		f["slider_tick_rate"] = "must be one of 0.5, 1, 2, 3, 4"
	}
	if s.Game != "" && s.Game != "osu" {
		f["game"] = `only "osu" is supported`
	}
	if len(f) > 0 {
		return &ValidationError{Fields: f}
	}
	return nil
}

// ValidationError is returned by Settings.Validate.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string {
	return fmt.Sprintf("automapper: invalid settings: %v", e.Fields)
}

// Quota is the daily allowance snapshot.
type Quota struct {
	Limit     int       `json:"limit"`
	Used      int       `json:"used"`
	InFlight  int       `json:"in_flight"`
	Remaining int       `json:"remaining"`
	ResetsAt  time.Time `json:"resets_at"`
}

// Job is the 202 answer to creating a map.
type Job struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	StatusURL string `json:"status_url"`
	MapURL    string `json:"map_url"`
	Quota     Quota  `json:"quota"`
}

// Map is a map's full state.
type Map struct {
	ID          string    `json:"id"`
	Status      string    `json:"status"` // queued, running, done or failed
	Game        string    `json:"game"`
	Format      string    `json:"format"`
	Title       string    `json:"title"`
	Artist      string    `json:"artist"`
	Mapper      string    `json:"mapper"`
	StarRating  float64   `json:"star_rating"`
	Year        int       `json:"year"`
	DownloadURL string    `json:"download_url"`
	MapURL      string    `json:"map_url"`
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
	FinishedAt  time.Time `json:"finished_at"`
	Position    int       `json:"position"` // while queued
	Queued      any       `json:"queued"`   // while queued (shape undocumented)
	Message     string    `json:"message"`  // why a map failed
	Hidden      bool      `json:"hidden"`   // flagged by moderation
}

// Done reports whether the map finished successfully.
func (m *Map) Done() bool { return m.Status == StatusDone }

// Finished reports whether the map is done or failed.
func (m *Map) Finished() bool { return m.Status == StatusDone || m.Status == StatusFailed }

// MapList is a page of public maps.
type MapList struct {
	Maps    []Map `json:"maps"`
	Page    int   `json:"page"`
	PerPage int   `json:"per_page"`
	Total   int   `json:"total"`
}

// Me is the answer of GET /me. Its exact shape is not documented, so Raw
// carries the full decoded body.
type Me struct {
	Quota Quota          `json:"quota"`
	Raw   map[string]any `json:"-"`
}

// APIError is a non-2xx answer from the API.
type APIError struct {
	Status     int               // HTTP status
	Code       string            // e.g. quota_exceeded, too_many_in_flight
	Message    string            // human readable
	Fields     map[string]string // per-field problems on 422
	RetryAfter time.Duration     // from Retry-After on 429
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("automapper: %d", e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if len(e.Fields) > 0 {
		s += fmt.Sprintf(" %v", e.Fields)
	}
	return s
}

// IsRateLimit reports whether err is any 429 answer (quota, in-flight, rate limit...).
func IsRateLimit(err error) bool { return hasStatus(err, http.StatusTooManyRequests) }

// IsQuotaExceeded reports whether today's allowance is used up.
func IsQuotaExceeded(err error) bool { return hasCode(err, "quota_exceeded") }

// IsUnauthorized reports a missing, wrong or revoked key.
func IsUnauthorized(err error) bool { return hasStatus(err, http.StatusUnauthorized) }

// IsNotFound reports a missing (or private) map.
func IsNotFound(err error) bool { return hasStatus(err, http.StatusNotFound) }

func hasStatus(err error, s int) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == s
}

func hasCode(err error, c string) bool {
	var e *APIError
	return errors.As(err, &e) && e.Code == c
}

// FailedError is returned by Wait/Generate when the server could not make the map.
type FailedError struct{ Map *Map }

func (e *FailedError) Error() string {
	return fmt.Sprintf("automapper: map %s failed: %s", e.Map.ID, e.Map.Message)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return parseError(resp, data)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("automapper: decoding response: %w", err)
	}
	return nil
}

func parseError(resp *http.Response, data []byte) error {
	e := &APIError{Status: resp.StatusCode}
	var body struct {
		Error  string            `json:"error"`
		Code   string            `json:"code"`
		Fields map[string]string `json:"fields"`
	}
	if json.Unmarshal(data, &body) == nil {
		e.Message, e.Code, e.Fields = body.Error, body.Code, body.Fields
	}
	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if n, err := strconv.Atoi(ra); err == nil {
			e.RetryAfter = time.Duration(n) * time.Second
		} else if t, err := http.ParseTime(ra); err == nil {
			e.RetryAfter = time.Until(t)
		}
	}
	return e
}

// Create uploads audio and queues a map. audio is streamed; filename sets the
// extension the server sees (mp3, ogg or wav).
func (c *Client) Create(ctx context.Context, audio io.Reader, filename string, s Settings) (*Job, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	settings, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			if err := mw.WriteField("settings", string(settings)); err != nil {
				return err
			}
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="audio"; filename=%q`, filepath.Base(filename)))
			h.Set("Content-Type", audioType(filename))
			part, err := mw.CreatePart(h)
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, audio); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	var job Job
	if err := c.do(ctx, http.MethodPost, "/maps", pr, mw.FormDataContentType(), &job); err != nil {
		pr.CloseWithError(err)
		return nil, err
	}
	return &job, nil
}

// CreateFromFile is Create reading the audio from a file path.
func (c *Client) CreateFromFile(ctx context.Context, path string, s Settings) (*Job, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return c.Create(ctx, f, path, s)
}

func audioType(name string) string {
	switch filepath.Ext(name) {
	case ".ogg":
		return "audio/ogg"
	case ".wav":
		return "audio/wav"
	default:
		return "audio/mpeg"
	}
}

// Get fetches a map's current state.
func (c *Client) Get(ctx context.Context, id string) (*Map, error) {
	var m Map
	if err := c.do(ctx, http.MethodGet, "/maps/"+url.PathEscape(id), nil, "", &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Event names passed to an OnUpdate callback.
const (
	EventSubmitted = "submitted" // upload accepted; Map.MapURL is already valid
	EventQueued    = "queued"    // waiting; Map.Position says where (fires when it changes)
	EventStarted   = "started"   // the map began generating
	EventDone      = "done"      // finished; Map.DownloadURL and Map.MapURL are set
	EventFailed    = "failed"    // Map.Message says why
)

// Update describes a change in a map's progress.
type Update struct {
	Event string
	Map   *Map
	Job   *Job // only set on EventSubmitted
}

type callOpts struct{ onUpdate func(Update) }

// CallOption customises Wait, Generate and GenerateAndSave.
type CallOption func(*callOpts)

// OnUpdate registers a callback for progress: submitted, queued (with
// position), started, then done (with the final URLs) or failed. It runs on
// the calling goroutine, so keep it quick.
func OnUpdate(fn func(Update)) CallOption { return func(o *callOpts) { o.onUpdate = fn } }

func (o *callOpts) emit(u Update) {
	if o.onUpdate != nil {
		o.onUpdate(u)
	}
}

func newCallOpts(opts []CallOption) *callOpts {
	o := &callOpts{}
	for _, f := range opts {
		f(o)
	}
	return o
}

// Wait polls until the map is done. It returns a *FailedError if the map failed.
func (c *Client) Wait(ctx context.Context, id string, opts ...CallOption) (*Map, error) {
	return c.wait(ctx, id, newCallOpts(opts))
}

func (c *Client) wait(ctx context.Context, id string, o *callOpts) (*Map, error) {
	lastStatus, lastPos := "", -1
	for {
		m, err := c.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		switch m.Status {
		case StatusQueued:
			if lastStatus != StatusQueued || m.Position != lastPos {
				o.emit(Update{Event: EventQueued, Map: m})
			}
		case StatusRunning:
			if lastStatus != StatusRunning {
				o.emit(Update{Event: EventStarted, Map: m})
			}
		case StatusDone:
			if lastStatus == StatusQueued {
				o.emit(Update{Event: EventStarted, Map: m}) // finished between polls
			}
			o.emit(Update{Event: EventDone, Map: m})
			return m, nil
		case StatusFailed:
			o.emit(Update{Event: EventFailed, Map: m})
			return m, &FailedError{Map: m}
		}
		lastStatus, lastPos = m.Status, m.Position
		select {
		case <-ctx.Done():
			return m, ctx.Err()
		case <-time.After(c.pollInterval):
		}
	}
}

// Generate does it all: upload the file, wait, and return the finished map
// (with DownloadURL and MapURL). Use ctx for a timeout; maps usually take one
// to three minutes. Pass OnUpdate to follow progress.
func (c *Client) Generate(ctx context.Context, audioPath string, s Settings, opts ...CallOption) (*Map, error) {
	o := newCallOpts(opts)
	job, err := c.CreateFromFile(ctx, audioPath, s)
	if err != nil {
		return nil, err
	}
	o.emit(Update{Event: EventSubmitted, Job: job, Map: &Map{
		ID: job.ID, Status: job.Status, MapURL: job.MapURL, Title: s.Title, Artist: s.Artist,
	}})
	return c.wait(ctx, job.ID, o)
}

// GenerateAndSave is Generate followed by downloading the result to destPath.
func (c *Client) GenerateAndSave(ctx context.Context, audioPath, destPath string, s Settings, opts ...CallOption) (*Map, error) {
	m, err := c.Generate(ctx, audioPath, s, opts...)
	if err != nil {
		return m, err
	}
	return m, c.DownloadFile(ctx, m, destPath)
}

// Download streams a finished map's file into w.
func (c *Client) Download(ctx context.Context, m *Map, w io.Writer) error {
	if m.DownloadURL == "" {
		return errors.New("automapper: map has no download_url (is it done?)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.DownloadURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return parseError(resp, data)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// DownloadFile saves a finished map to path. Nothing is written on failure.
func (c *Client) DownloadFile(ctx context.Context, m *Map, path string) error {
	var buf bytes.Buffer
	if err := c.Download(ctx, m, &buf); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// SearchOptions filters Search. Zero values use server defaults.
type SearchOptions struct {
	Query   string // title/artist/mapper/tags, or 8+ hex chars of a SHA-256
	Page    int
	PerPage int // max 50
}

// Search lists public maps, newest first.
func (c *Client) Search(ctx context.Context, o SearchOptions) (*MapList, error) {
	q := url.Values{}
	if o.Query != "" {
		q.Set("q", o.Query)
	}
	if o.Page > 0 {
		q.Set("page", strconv.Itoa(o.Page))
	}
	if o.PerPage > 0 {
		q.Set("per_page", strconv.Itoa(o.PerPage))
	}
	path := "/maps"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var l MapList
	if err := c.do(ctx, http.MethodGet, path, nil, "", &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// Me returns who you are and today's usage.
func (c *Client) Me(ctx context.Context) (*Me, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/me", nil, "", &raw); err != nil {
		return nil, err
	}
	var me Me
	if err := json.Unmarshal(raw, &me); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(raw, &me.Raw)
	return &me, nil
}
