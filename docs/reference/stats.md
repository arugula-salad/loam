# Stats

How much your app is used: loam counts every request to a sandbox's URLs as it passes them on,
per day, without your app doing anything. Asleep or awake, under `*.loam.example.com` or your
project's own domain, every request is counted.

| Method and path | |
|---|---|
| `GET /v1/sandboxes/{sandbox}/stats?days=N` | → `Stats`: the last `N` days, today (UTC) last. `N` is 1 to 90, 7 if you leave it out. |

```console
$ curl -sS -H "Authorization: Bearer $LOAM_TOKEN" "$LOAM_API_URL/v1/sandboxes/web/stats?days=2"
{
  "sandbox": "sb-x4cytxhpcior",
  "name": "web",
  "days": [
    {"day": "2026-10-12", "requests": 1840, "status_2xx": 1702, "status_3xx": 41, "status_4xx": 88,
     "status_5xx": 3, "websockets": 6, "bytes_sent": 51203311, "key_requests": 12, "visitors": 97},
    {"day": "2026-10-13", "requests": 412, "status_2xx": 390, "status_3xx": 9, "status_4xx": 13,
     "status_5xx": 0, "websockets": 0, "bytes_sent": 10022130, "key_requests": 0, "visitors": 31}
  ],
  "total": {"requests": 2252, "status_2xx": 2092, "status_3xx": 50, "status_4xx": 101,
            "status_5xx": 3, "websockets": 6, "bytes_sent": 61225441, "key_requests": 12},
  "visitor_days": 128
}
```

```console
$ loam stats web --days 2
DAY         VISITORS  REQUESTS  2XX   3XX  4XX  5XX  WS  SENT     KEY
2026-10-12  97        1840      1702  41   88   3    6   48.8MiB  12
2026-10-13  31        412       390   9    13   0    0   9.6MiB   0
total       128*      2252      2092  50   101  3    6   58.4MiB  12
* visitor-days: a visitor on two days counts twice
```

Every day of the window is there, with zeros on a day nobody came. A read key may ask, so a
dashboard or an agent can hold one without being able to change anything.

## What counts

| Field | |
|---|---|
| `requests` | every request, including a WebSocket's upgrade and loam's own refusals |
| `status_2xx` .. `status_5xx` | requests by the status the client got: your app's, or loam's own (401 on a private port without the key, 502 `port_closed` when nothing listens) |
| `websockets` | WebSocket upgrades; these are in no status class |
| `bytes_sent` | response bodies, not counting WebSockets |
| `key_requests` | requests that carried the project's key: every answered request to a private port, and a request to a public port with the key (bearer token, `loam_token` cookie, or `?loam_token=`). That's you and your tools, not your users. |
| `visitors` | the day's distinct visitors (below) |
| `visitor_days` | the days' `visitors` added up |

### Visitors

A visitor is a client loam hasn't seen on that sandbox that day: its address and its browser (its
`User-Agent`) together. A request counts as a visit when:

- it got an answer below 400 (a WebSocket's 101 included);
- it didn't carry the project's key;
- its `User-Agent` isn't empty and isn't a known machine: crawlers and bots, link previews, uptime
  probes, and HTTP libraries such as curl, wget, Python's requests or Go's client.

No addresses are kept. loam stores a hash of a random salt, the sandbox and the client, and
deletes the salt when the day ends, so a visitor can't be recognised on another day.
The cost of that: someone who comes on three days counts three times in `visitor_days`. Some
counts are off at the edges. Two people behind one address with the same browser are one
visitor. A phone that changes networks during the day is two.

Visitors are people who reach your URL. They aren't signed-up users. If your app has accounts,
your app's own database is where to count them.

## When

Counts are written every 30 seconds; a stats call writes what's waiting first, so today's numbers
are current to the call. Days are UTC. loam keeps 90 days, and a sandbox's traffic is deleted with
the sandbox.

## Go and TypeScript

```go
st, err := c.Stats(ctx, "web", 30)
if err != nil {
	log.Fatal(err)
}
fmt.Println(st.Days[len(st.Days)-1].Visitors, "visitors today;", st.VisitorDays, "visitor-days in 30 days")
```

```ts
const st = await loam.sandbox('web').stats({ days: 30 })
console.log(st.days.at(-1)?.visitors, 'visitors today')
```
