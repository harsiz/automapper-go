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

## All you need

Five required fields and your key. Everything else is optional.

```go
c := automapper.New(os.Getenv("AUTOMAPPER_KEY"))
c.GenerateAndSave(ctx, "song.mp3", "map.osz", automapper.Settings{
	Format: automapper.FormatOsz, Title: "Song", Artist: "Artist", StarRating: 5, Year: 2018,
})
```

## Optional fields

Set any of these on `automapper.Settings`; leave them out to use the defaults.

| Field | Type | What it does |
| --- | --- | --- |
| `ArtistRomanised` | `string` | Artist in Latin letters (defaults to `Artist`) |
| `Mapper` | `string` | Creator name in the map (defaults to `AutoMapper`) |
| `Tags` | `string` | Space separated tags |
| `MapperID` | `int` | osu! user id whose mapping style to imitate |
| `HP`, `AR`, `CS`, `OD` | `*float64` | 0 to 10. Leave `nil` to let the model pick (`automapper.Float(8)`) |
| `SliderTickRate` | `float64` | One of `0.5`, `1`, `2`, `3`, `4` |
| `Background` | `*bool` | Anime wallpaper behind `osz` maps, on by default (`automapper.Bool(false)` to turn off) |
| `GoAstray` | `bool` | Unlocks `CfgScale` and `Descriptors` |
| `CfgScale` | `*float64` | 1 to 3, needs `GoAstray` |
| `Descriptors` | `[]string` | e.g. `[]string{"jumps/wide"}`, needs `GoAstray` |
| `Game` | `string` | Only `"osu"` (the default) works for now |

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
