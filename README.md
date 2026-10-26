# automapper-go

Go client for the [osu! AutoMapper API](https://am.justharsiz.lol). Zero dependencies.

```bash
go get github.com/harsiz/automapper-go
```

Get a key from your account page on the site (sign in with osu!), then:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	automapper "github.com/harsiz/automapper-go"
)

func main() {
	c := automapper.New(os.Getenv("AUTOMAPPER_KEY"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	m, err := c.GenerateAndSave(ctx, "song.mp3", "blue-zenith.osz", automapper.Settings{
		Format:     automapper.FormatOsz,
		Title:      "Blue Zenith",
		Artist:     "xi",
		StarRating: 6.5,
		Year:       2020,
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("done:", m.MapURL)
}
```

`GenerateAndSave` uploads, polls until finished, and downloads. Maps usually take 1–3 minutes, so give the context a generous timeout.

## Step by step

```go
job, err := c.CreateFromFile(ctx, "song.mp3", settings) // or c.Create(ctx, reader, "song.mp3", settings)
m, err := c.Wait(ctx, job.ID)                            // polls every 5s (WithPollInterval to change)
err = c.DownloadFile(ctx, m, "map.osz")                  // or c.Download(ctx, m, writer)
```

Other calls: `Get(ctx, id)`, `Search(ctx, SearchOptions{Query: "zenith"})`, `Me(ctx)`.

## Settings

Required: `Format` (`FormatOsz` / `FormatOsu`), `Title`, `Artist`, `StarRating` (2–12), `Year` (2007–2023).
Optional fields mirror the API docs; `HP/AR/CS/OD/CfgScale/Background` are pointers so "unset" differs from zero — use `automapper.Float(8)` and `automapper.Bool(false)`.
Settings are validated locally before anything is uploaded.

## Errors

```go
m, err := c.Generate(ctx, "song.mp3", s)
var apiErr *automapper.APIError
var failed *automapper.FailedError
switch {
case automapper.IsQuotaExceeded(err):  // today's 50 are used
case errors.As(err, &apiErr):          // apiErr.Status, .Code, .Fields, .RetryAfter
case errors.As(err, &failed):          // the server could not map this song: failed.Map.Message
}
```

Also `IsRateLimit`, `IsUnauthorized`, `IsNotFound`.

Keep your key server-side; the API intentionally blocks cross-origin requests.
