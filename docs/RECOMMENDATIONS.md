# Recommendations (hybrid-1)

A hybrid ranker: what the listener said, what they did, what the catalogue says about each song, and how songs do with
listeners as a whole. No collaborative filtering, no audio analysis, no models to train.

Code: `internal/recommend`. Every weight and threshold: `config.go`. Change them together with `Version`.
**They are starting values chosen by judgement, not fitted to data.** Tune them from the outcome report.

## Where it is used
| Surface | Route | Notes |
|---|---|---|
| Start mix, autoplay | `POST /v1/mixes` | same request and answer as before. If ranking fails, the old same-genre query answers |
| Your sound | `GET /v1/me/your-sound`, worker refresh | new songs only; playlist upkeep (size, daily additions, never removing the listener's songs) unchanged |

The app needs no change.

## Data foundation (migration `0037_recommendations.sql`)
| Object | What | Fresh |
|---|---|---|
| `rec_track_features` (view) | playable public songs: genre(s), **verified** languages/regions/cultures/mood-energy-etc. | live |
| `rec_user_tracks` (view) | one definition of a listener's history with each song | live |
| `rec_track_stats` (table) | listeners 7/30 days, finished and skipped listener-days (90), savers; built only from `signal_*` views | worker, every 15 min |
| `rec_serves`, `rec_serve_items` | what was suggested: surface, version, mode, position, slot, score, reasons | on each list; 90 days; deleted with the account |
| `rec_outcomes` (view) | the first matching listen after each suggestion, and whether it was liked or added | live |

History rules: 180 days of listening, weights halve every 45 days. A skip under 30% counts fully, a later one half.
Skips with `capture < 2` (old app) are **weak**, because some are false. Playlist songs count only if the listener put
them there (not the server, not "Your sound"). Counted plays matter only where there are no listen events. An artist's
listening to their own music is ignored.

Metadata rules: pending and rejected values do not count. Genre is the exception: the uploader's genre counts unless
rejected, as do other genre links not rejected. Missing metadata never counts against a song.

## Ranking
1. **Profile** (per request, not stored): affinity per artist and genre (-1..1) from behaviour, plus favourite and
   followed artists and chosen genres. The set-up choices weigh most while there is little behaviour. Language, region
   and sound taste are also learned from verified metadata of songs enjoyed. No culture is worked out for a listener.
2. **Candidates** (about 1,200 at most): the listener's artists, genres, languages and regions (the starting song's for
   a mix), popular, newest and a random handful. Never the listener's own music.
3. **Score** = artist .28 + genre .20 (related genres .6) + language .20 + region .07 + sound .05 + popularity .08
   + finish-vs-skip ±.07 + newness .05; then +.08 liked/saved, +.06 on repeat; minus recently heard (.60 within a day,
   down to .08 within 14 days) and recent skips (.50 week, .25 month, .10 weak).
   Language: preferred 1.0 (secondary .6), understood .4 (.25), learned from listening up to .5.
4. **Never offered:** a blocked song or artist (see below); unliked and not saved; or skipped 2+ times (3+ counting weak) in 90 days, never finished, not kept.
5. **Mix:** a song with a genre gets only that genre or a related one. Without one: same artist, or shared verified
   language, culture or region. Order = 55% likeness to the starting song + 45% the score above.
6. **Your sound:** only songs with something of the listener's; not saved, not heard 30 days, not offered 90 days.
7. **Discovery places** (artists with no history, not favourite, not followed):

| Mode | Share | New artist needs (Your sound) | Other |
|---|---|---|---|
| familiar | 15% | match ≥ .30 | language weight x1.2 |
| explore_more (default) | 30% | match ≥ .05 | +.15 for a new verified language/region |
| everything_uganda | 45% | any match, or new ground | +.30 for new ground; all verified regions weigh alike; language x0.8 |

   Within them songs rank by match .45, finish rate .20, little-heard artist .20, newness .15.
8. **Diversity:** at most 4 per artist in Your sound, about 12% of a mix (min 2); two other songs between an artist's
   songs where possible; each repeat is marked down. A mix passes the limit rather than come up short.
9. Order varies a little: each time for a mix, daily for Your sound.

## Blocking (migration `0039_recommendation_blocks.sql`)
Administrators keep a song, or an artist (including songs added later), out of mixes, autoplay and Your sound.
It still plays, appears in search and stays in listeners' own playlists. Enforced where candidates are chosen
(`recommend.Candidates`), in the fallback same-genre mix, and by the Your sound refresh, all through the view
`rec_blocked_tracks`. Blocking at once removes what the **server** had put in Your sound (never the listener's own
additions). Browse, trending, popular and related lists are not affected. Featured credits are not covered: an
artist block covers songs where they are the main artist.

`GET|POST /v1/admin/recommendations/blocks`, `DELETE /v1/admin/recommendations/blocks/{id}` (audit:
`recommendation.block`, `recommendation.unblock`). Any new suggestion list must also leave out `rec_blocked_tracks`.

## Looking into it (administrators)
| Route | Answers |
|---|---|
| `GET /v1/admin/users/{id}/recommendations?surface=your_sound\|mix&seed=` | what this listener would get, with reasons, score parts and the basis used. Records nothing |
| `GET /v1/admin/recommendations/outcomes?days=14` | per surface, version and slot: suggested, listened, completed, skipped, kept |
| `GET /v1/admin/recommendations/readiness` | songs with each kind of verified metadata; listeners with enough to go on |

Compare `familiar` with `discovery`, and one version with the next. Suggested-but-not-listened is not a rejection (the
list may not have been played that far). Listens are matched by surface only for apps sending `capture >= 2`; an old
app's listen of the same song counts however it was found. Your sound outcomes match any playlist listen.

## Limits
- With no verified language, region, culture or mood, those preferences have no effect. Check `readiness`.
- Listeners are not asked for a culture; track cultures are used only for likeness in mixes.
- "Everything Uganda" cannot tell Ugandan music apart without verified regions; it mainly raises the new-artist share.
- The same recording on two releases can appear twice (no ISRC).
- No home-screen recommendations feed yet; browse, trending and related lists are unchanged.

## Cost
On a developer PC with 5,000 songs and 100,000 listens: about 50 ms per mix or Your sound list, 0.4 s per statistics
rebuild. Not measured on the production server. The worker warns when a rebuild takes over 10 s.
