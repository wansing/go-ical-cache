package icalcache

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
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
	Start   time.Time
	End     time.Time
	UID     string
	URL     string
	Summary string
}

type Cache struct {
	Config
	Interval     time.Duration // default is two minutes
	events       []Event
	lastChecked  time.Time
	lastModified time.Time
	lock         sync.RWMutex
}

// Get returns all events. The defaultLocation parameter is used if the ical data contains no TZID location.
func (cache *Cache) Get(defaultLocation *time.Location) ([]Event, error) {
	if cache.URL == "" {
		return nil, nil
	}

	if cache.Interval < 30*time.Second { // see also http client timeout
		cache.Interval = 2 * time.Minute
	}

	if time.Since(cache.lastChecked) < cache.Interval {
		return cache.events, nil
	}

	// first call takes the write lock and does the job, subsequent calls wait until the job is finished
	if cache.lock.TryLock() {
		defer cache.lock.Unlock()

		req, err := http.NewRequest(http.MethodHead, cache.URL, nil)
		if err != nil {
			return nil, err
		}
		if cache.Config.Username != "" {
			req.SetBasicAuth(cache.Config.Username, cache.Config.Password)
		}
		if t, ok := client.Transport.(*http.Transport); ok {
			t.TLSClientConfig.InsecureSkipVerify = cache.SkipTLSVerify
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}

		if lastModified, err := time.Parse("Mon, 02 Jan 2006 15:04:05 GMT", resp.Header.Get("Last-Modified")); err == nil {
			if lastModified.Before(cache.lastModified) || lastModified.Equal(cache.lastModified) {
				return cache.events, nil
			}
			cache.lastModified = lastModified
		}

		req, err = http.NewRequest(http.MethodGet, cache.URL, nil)
		if err != nil {
			return nil, err
		}
		if cache.Config.Username != "" {
			req.SetBasicAuth(cache.Config.Username, cache.Config.Password)
		}

		resp, err = client.Do(req)
		if err != nil {
			return nil, err
		}

		cal, err := ical.NewDecoder(resp.Body).Decode()
		if err == io.EOF { // no calendars in file
			cache.events = nil
			return cache.events, nil
		}
		if err != nil {
			return nil, err
		}

		var events []Event
		for _, event := range cal.Events() {
			uid, err := event.Props.Text(ical.PropUID)
			if err != nil {
				return nil, fmt.Errorf("getting uid: %w", err)
			}
			summary, err := event.Props.Text(ical.PropSummary)
			if err != nil {
				return nil, fmt.Errorf("getting summary: %w", err)
			}
			url, err := event.Props.URI(ical.PropURL)
			if err != nil {
				return nil, fmt.Errorf("getting url: %w", err)
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

			// go-ical "use[s] the TZID location, if available"
			start, err := event.DateTimeStart(defaultLocation)
			if err != nil {
				return nil, fmt.Errorf("getting start time: %w", err)
			}
			end, err := event.DateTimeEnd(defaultLocation)
			if err != nil {
				return nil, fmt.Errorf("getting end time: %w", err)
			}

			var urlString string
			if url != nil {
				urlString = url.String()
			}

			events = append(events, Event{
				UID:     uid,
				Summary: summary,
				URL:     urlString,
				Start:   start,
				End:     end,
			})
		}

		cache.events = events
		cache.lastChecked = time.Now()
	} else {
		// wait until write lock is released
		cache.lock.RLock()
		cache.lock.RUnlock()
	}

	return cache.events, nil
}
