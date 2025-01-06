# go-ical-cache

[![Go Reference](https://pkg.go.dev/badge/github.com/wansing/go-ical-cache.svg)](https://pkg.go.dev/github.com/wansing/go-ical-cache)

Package `icalcache` provides a caching iCalendar client. It does up to one HTTP HEAD request every `Interval`. If the `Last-Modified` header has changed, then the whole feed is fetched from upstream.
