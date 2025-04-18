package icalcache

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/emersion/go-ical"
)

var client = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			// InsecureSkipVerify:
		},
	},
}

type Config struct {
	URL           string `json:"url"`
	Username      string `json:"username"` // optional
	Password      string `json:"password"` // optional
	SkipTLSVerify bool   `json:"skip-tls-verify"`
}

func LoadConfig(jsonfile string) (Config, error) {
	filecontent, err := os.ReadFile(jsonfile)
	if err != nil {
		return Config{}, fmt.Errorf("error opening ical config: %v", err)
	}
	var config Config
	if err := json.Unmarshal(filecontent, &config); err != nil {
		return Config{}, fmt.Errorf("error decoding ical config: %v", err)
	}
	return config, nil
}

type Event struct {
	AllDay         bool
	Start          time.Time
	End            time.Time
	RecurrenceRule string
	UID            string
	URL            string
	Summary        string
	Description    string
}

type Cache struct {
	Config
	Interval time.Duration // default is two minutes

	lock         sync.Mutex
	events       []Event
	lastChecked  time.Time
	lastHashSum  string
	lastModified int64
}

// Get returns all events. The defaultLocation parameter is used if the ical data contains no TZID location.
func (cache *Cache) Get(defaultLocation *time.Location) ([]Event, int64, error) {
	// check cache configuration
	if cache.URL == "" {
		return nil, 0, nil
	}
	if cache.Interval < 30*time.Second { // see also http client timeout
		cache.Interval = 2 * time.Minute
	}

	// If a function call fetches from upstream, subsequent calls have to wait. (Else they would always get stale data in scenarios with frequent upstream changes and few calls.)
	cache.lock.Lock()
	defer cache.lock.Unlock()

	// skip if upstream has recently been checked
	if time.Since(cache.lastChecked) < cache.Interval {
		return cache.events, cache.lastModified, nil
	}
	cache.lastChecked = time.Now()

	// HTTP HEAD upstream
	req, err := http.NewRequest(http.MethodHead, cache.URL, nil)
	if err != nil {
		return cache.events, cache.lastModified, fmt.Errorf("making upstream header request: %w", err)
	}
	if cache.Config.Username != "" {
		req.SetBasicAuth(cache.Config.Username, cache.Config.Password)
	}
	if t, ok := client.Transport.(*http.Transport); ok {
		t.TLSClientConfig.InsecureSkipVerify = cache.SkipTLSVerify
	}
	resp, err := client.Do(req)
	if err != nil {
		return cache.events, cache.lastModified, fmt.Errorf("getting upstream headers: %w", err)
	}

	// skip if upstream has a Last-Modified header whose value is older
	var httpLastModifiedWasAvailable = false
	if httpLastModified, err := time.Parse("Mon, 02 Jan 2006 15:04:05 GMT", resp.Header.Get("Last-Modified")); err == nil {
		httpLastModifiedWasAvailable = true
		if httpLastModified.Unix() <= cache.lastModified { // http timestamp before or equal cache timestamp
			return cache.events, cache.lastModified, nil
		}
		cache.lastModified = httpLastModified.Unix()
	}

	// HTTP GET upstream
	req, err = http.NewRequest(http.MethodGet, cache.URL, nil)
	if err != nil {
		return cache.events, cache.lastModified, fmt.Errorf("making upstream request: %w", err)
	}
	if cache.Config.Username != "" {
		req.SetBasicAuth(cache.Config.Username, cache.Config.Password)
	}
	resp, err = client.Do(req)
	if err != nil {
		return cache.events, cache.lastModified, fmt.Errorf("getting upstream data: %w", err)
	}

	// parse response body as ical and also hash it
	hash := fnv.New64()
	cal, err := ical.NewDecoder(io.TeeReader(resp.Body, hash)).Decode()
	if err == io.EOF { // no calendars in file
		cache.events = nil
		return cache.events, cache.lastModified, nil
	}
	if err != nil {
		return cache.events, cache.lastModified, fmt.Errorf("decoding upstream ical data: %w", err)
	}

	// update lastHashSum (which is a fallback if HTTP Last-Modified header is missing), update lastModified if the HTTP Last-Modified header was missing
	hashSum := base64.StdEncoding.EncodeToString(hash.Sum(nil))
	if !httpLastModifiedWasAvailable {
		if hashSum != cache.lastHashSum {
			cache.lastModified = time.Now().Unix() // only if upstream did not send a Last-Modified HTTP header (else time.Now() competes with upcoming upstream Last-Modified timestamps)
		}
	}
	cache.lastHashSum = hashSum

	// Update events. If an error occurs, we return an empty event list because that's better than an incomplete list.
	cache.events = cache.events[:0]
	for _, event := range cal.Events() {
		uid, err := event.Props.Text(ical.PropUID)
		if err != nil {
			return nil, 0, fmt.Errorf("getting uid: %w", err)
		}
		summary, err := event.Props.Text(ical.PropSummary)
		if err != nil {
			return nil, 0, fmt.Errorf("getting summary: %w", err)
		}
		description, err := event.Props.Text(ical.PropDescription)
		if err != nil {
			return nil, 0, fmt.Errorf("getting description: %w", err)
		}
		url, err := event.Props.URI(ical.PropURL)
		if err != nil {
			return nil, 0, fmt.Errorf("getting url: %w", err)
		}

		// replace TZIDs which can't be loaded by time.LoadLocation (workaround for https://github.com/emersion/go-ical/issues/10) with target location
		for _, propid := range []string{ical.PropDateTimeStart, ical.PropDateTimeEnd} {
			prop := event.Props.Get(propid)
			if prop != nil {
				// similar to https://github.com/emersion/go-ical/blob/fc1c9d8fb2b6/ical.go#L149C6-L149C58
				if tzid := prop.Params.Get(ical.PropTimezoneID); tzid != "" {
					_, err := time.LoadLocation(tzid)
					if err != nil {
						prop.Params.Set(ical.PropTimezoneID, defaultLocation.String())
					}
				}
			}
		}

		var allDay = false
		if startProp := event.Props.Get(ical.PropDateTimeStart); startProp != nil {
			if startProp.ValueType() == ical.ValueDate {
				allDay = true
			}
		}

		// go-ical "use[s] the TZID location, if available"
		start, err := event.DateTimeStart(defaultLocation)
		if err != nil {
			return nil, 0, fmt.Errorf("getting start time: %w", err)
		}
		end, err := event.DateTimeEnd(defaultLocation)
		if err != nil {
			return nil, 0, fmt.Errorf("getting end time: %w", err)
		}

		var rrule string
		if rOption, err := event.Props.RecurrenceRule(); err != nil {
			return nil, 0, fmt.Errorf("getting end recurrence rule: %w", err)
		} else if rOption != nil {
			rrule = rOption.String()
		}

		var urlString string
		if url != nil {
			urlString = url.String()
		}

		cache.events = append(cache.events, Event{
			AllDay:         allDay,
			Start:          start,
			End:            end,
			RecurrenceRule: rrule,
			UID:            uid,
			URL:            urlString,
			Summary:        summary,
			Description:    description,
		})
	}

	return cache.events, cache.lastModified, nil
}
